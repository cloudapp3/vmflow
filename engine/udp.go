package engine

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultUDPRuleMaxSessions is the per-rule fallback when max_conn is zero.
	// Each UDP session owns a socket and receive goroutine, so zero cannot mean
	// unbounded without exposing the process to trivial resource exhaustion.
	DefaultUDPRuleMaxSessions = DefaultUDPGlobalMaxSessions

	udpMaxDatagramSize     = 64 * 1024
	udpReadTimeout         = 5 * time.Second
	udpWriteTimeout        = time.Second
	udpUploadQueueCapacity = 64
	udpUploadQueueMaxBytes = udpMaxDatagramSize
)

type udpRunner struct {
	rule      Rule
	stats     boundRuleStats
	target    *net.UDPAddr
	ipMatcher *sourceIPMatcher
	policyErr error

	ctx    context.Context
	cancel context.CancelFunc
	ln     *net.UDPConn

	mu       sync.Mutex
	sessions map[netip.AddrPort]*udpSession
	budget   *udpSessionBudget
	wg       sync.WaitGroup

	clientWriteMu    sync.Mutex
	clientWriteUntil time.Time
}

type udpSession struct {
	targetConn *net.UDPConn
	clientAddr netip.AddrPort
	lastSeen   atomic.Int64
	closed     atomic.Bool
	readUntil  time.Time
	writeUntil time.Time
	ctx        context.Context
	cancel     context.CancelFunc
	budget     *udpSessionBudget

	uploadLimiter   *rateLimiter
	downloadLimiter *rateLimiter
	uploadQueue     chan *udpQueuedPacket
	queuedBytes     atomic.Int64
	pendingDownload atomic.Int64
}

type udpQueuedPacket struct {
	data           []byte
	poolClass      int
	accountedBytes int64
}

var udpPacketPools = [...]sync.Pool{
	{New: func() any { return &udpQueuedPacket{data: make([]byte, 256), poolClass: 0} }},
	{New: func() any { return &udpQueuedPacket{data: make([]byte, 1024), poolClass: 1} }},
	{New: func() any { return &udpQueuedPacket{data: make([]byte, 4096), poolClass: 2} }},
	{New: func() any { return &udpQueuedPacket{data: make([]byte, 16*1024), poolClass: 3} }},
	{New: func() any { return &udpQueuedPacket{data: make([]byte, udpMaxDatagramSize), poolClass: 4} }},
}

// Keeping this buffer off the goroutine stack avoids the next stack-size class
// (about 128 KiB per active session) while retaining full-size UDP datagrams.
var udpReadBufferPool = sync.Pool{
	New: func() any { return new([udpMaxDatagramSize]byte) },
}

func newUDPRunner(rule Rule, collector *Collector) Runner {
	return newUDPRunnerWithBudget(rule, collector, newUDPSessionBudget(DefaultUDPGlobalMaxSessions))
}

func newUDPRunnerWithBudget(rule Rule, collector *Collector, budget *udpSessionBudget) Runner {
	ctx, cancel := context.WithCancel(context.Background())
	matcher, policyErr := compileSourceIPMatcher(rule.SourceIPMode, rule.SourceIPs)
	if budget == nil {
		budget = newUDPSessionBudget(DefaultUDPGlobalMaxSessions)
	}
	return &udpRunner{
		rule:      rule,
		stats:     collector.bindRule(rule.RuleID),
		ipMatcher: matcher,
		policyErr: policyErr,
		ctx:       ctx,
		cancel:    cancel,
		sessions:  make(map[netip.AddrPort]*udpSession),
		budget:    budget,
	}
}

func (runner *udpRunner) Start() error {
	if runner.policyErr != nil {
		return runner.policyErr
	}
	listenAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(strings.TrimSpace(runner.rule.ListenAddr), strconv.Itoa(runner.rule.ListenPort)))
	if err != nil {
		return err
	}
	targetAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(strings.TrimSpace(runner.rule.TargetAddr), strconv.Itoa(runner.rule.TargetPort)))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return err
	}
	runner.target = targetAddr
	runner.ln = conn
	runner.wg.Add(2)
	go runner.readLoop()
	go runner.cleanupLoop()
	return nil
}

func (runner *udpRunner) Stop() {
	runner.cancel()
	if runner.ln != nil {
		_ = runner.ln.Close()
	}
	runner.mu.Lock()
	for key, session := range runner.sessions {
		runner.closeSessionLocked(key, session)
	}
	runner.mu.Unlock()
	runner.wg.Wait()
}

func (runner *udpRunner) readLoop() {
	defer runner.wg.Done()
	buf := make([]byte, udpMaxDatagramSize)
	for {
		n, clientAddr, err := runner.ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-runner.ctx.Done():
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		runner.handlePacket(clientAddr, buf[:n])
	}
}

func (runner *udpRunner) handlePacket(clientAddr netip.AddrPort, payload []byte) {
	if !clientAddr.IsValid() {
		return
	}
	if runner.ipMatcher != nil && !runner.ipMatcher.allows(clientAddr.Addr()) {
		runner.stats.incSourceIPDenied()
		return
	}
	key := clientAddr
	now := time.Now().UnixNano()

	runner.mu.Lock()
	session, ok := runner.sessions[key]
	if !ok {
		select {
		case <-runner.ctx.Done():
			runner.mu.Unlock()
			return
		default:
		}
		if len(runner.sessions) >= runner.sessionLimit() || !runner.budget.tryAcquire() {
			runner.stats.incUDPSessionRejected()
			runner.mu.Unlock()
			return
		}
		targetConn, err := net.DialUDP("udp", nil, runner.target)
		if err != nil {
			runner.budget.release()
			runner.mu.Unlock()
			return
		}
		sessionCtx, sessionCancel := context.WithCancel(runner.ctx)
		session = &udpSession{
			targetConn: targetConn,
			clientAddr: clientAddr,
			ctx:        sessionCtx,
			cancel:     sessionCancel,
			budget:     runner.budget,
		}
		if runner.rule.SpeedLimit > 0 {
			session.uploadLimiter = newRateLimiter(runner.rule.SpeedLimit)
			session.downloadLimiter = newRateLimiter(runner.rule.SpeedLimit)
			session.uploadQueue = make(chan *udpQueuedPacket, udpUploadQueueCapacity)
		}
		session.lastSeen.Store(now)
		runner.sessions[key] = session
		runner.stats.incConns()
		runner.wg.Add(1)
		go runner.readFromTarget(key, session)
		if session.uploadQueue != nil {
			runner.wg.Add(1)
			go runner.uploadLoop(key, session)
		}
	}
	session.lastSeen.Store(now)
	runner.mu.Unlock()

	if session.uploadQueue != nil {
		runner.enqueueUpload(session, payload)
		return
	}
	if err := runner.writeToTarget(session, payload); err != nil {
		runner.removeSession(key, session)
		return
	}
	runner.stats.addUpload(int64(len(payload)))
}

func (runner *udpRunner) sessionLimit() int {
	if runner.rule.MaxConn > 0 {
		return runner.rule.MaxConn
	}
	return DefaultUDPRuleMaxSessions
}

func (runner *udpRunner) enqueueUpload(session *udpSession, payload []byte) {
	accountedBytes := udpQueuedPacketCapacity(len(payload))
	for {
		queued := session.queuedBytes.Load()
		if queued+accountedBytes > udpUploadQueueMaxBytes {
			runner.stats.incUDPPacketsDropped()
			return
		}
		if session.queuedBytes.CompareAndSwap(queued, queued+accountedBytes) {
			break
		}
	}

	packet := acquireUDPQueuedPacket(payload)
	select {
	case <-session.ctx.Done():
		session.queuedBytes.Add(-packet.accountedBytes)
		releaseUDPQueuedPacket(packet)
	case session.uploadQueue <- packet:
	default:
		session.queuedBytes.Add(-packet.accountedBytes)
		runner.stats.incUDPPacketsDropped()
		releaseUDPQueuedPacket(packet)
	}
}

func (runner *udpRunner) uploadLoop(key netip.AddrPort, session *udpSession) {
	defer runner.wg.Done()
	defer runner.drainUploadQueue(session)
	for {
		select {
		case <-session.ctx.Done():
			return
		case packet := <-session.uploadQueue:
			payload := packet.data
			if err := session.uploadLimiter.wait(session.ctx, len(payload)); err != nil {
				session.queuedBytes.Add(-packet.accountedBytes)
				releaseUDPQueuedPacket(packet)
				return
			}
			if err := runner.writeToTarget(session, payload); err != nil {
				session.queuedBytes.Add(-packet.accountedBytes)
				releaseUDPQueuedPacket(packet)
				runner.removeSession(key, session)
				return
			}
			runner.stats.addUpload(int64(len(payload)))
			session.lastSeen.Store(time.Now().UnixNano())
			session.queuedBytes.Add(-packet.accountedBytes)
			releaseUDPQueuedPacket(packet)
		}
	}
}

func (runner *udpRunner) drainUploadQueue(session *udpSession) {
	for {
		select {
		case packet := <-session.uploadQueue:
			session.queuedBytes.Add(-packet.accountedBytes)
			releaseUDPQueuedPacket(packet)
		default:
			return
		}
	}
}

func acquireUDPQueuedPacket(payload []byte) *udpQueuedPacket {
	if len(payload) > udpMaxDatagramSize {
		data := make([]byte, len(payload))
		copy(data, payload)
		return &udpQueuedPacket{data: data, poolClass: -1, accountedBytes: int64(cap(data))}
	}
	poolClass := len(udpPacketPools) - 1
	switch size := len(payload); {
	case size <= 256:
		poolClass = 0
	case size <= 1024:
		poolClass = 1
	case size <= 4096:
		poolClass = 2
	case size <= 16*1024:
		poolClass = 3
	}
	packet := udpPacketPools[poolClass].Get().(*udpQueuedPacket)
	packet.data = packet.data[:len(payload)]
	packet.accountedBytes = int64(cap(packet.data))
	copy(packet.data, payload)
	return packet
}

func udpQueuedPacketCapacity(size int) int64 {
	switch {
	case size <= 256:
		return 256
	case size <= 1024:
		return 1024
	case size <= 4096:
		return 4096
	case size <= 16*1024:
		return 16 * 1024
	case size <= udpMaxDatagramSize:
		return udpMaxDatagramSize
	default:
		return int64(size)
	}
}

func releaseUDPQueuedPacket(packet *udpQueuedPacket) {
	if packet == nil || packet.poolClass < 0 || packet.poolClass >= len(udpPacketPools) {
		return
	}
	packet.data = packet.data[:cap(packet.data)]
	udpPacketPools[packet.poolClass].Put(packet)
}

func (runner *udpRunner) writeToTarget(session *udpSession, payload []byte) error {
	if err := refreshUDPWriteDeadline(session, time.Now()); err != nil {
		return err
	}
	_, err := session.targetConn.Write(payload)
	return err
}

func (runner *udpRunner) readFromTarget(key netip.AddrPort, session *udpSession) {
	defer runner.wg.Done()
	buf := udpReadBufferPool.Get().(*[udpMaxDatagramSize]byte)
	defer udpReadBufferPool.Put(buf)
	for {
		if err := refreshUDPReadDeadline(session, time.Now()); err != nil {
			runner.removeSession(key, session)
			return
		}
		n, err := session.targetConn.Read(buf[:])
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				select {
				case <-session.ctx.Done():
					runner.removeSession(key, session)
					return
				default:
					continue
				}
			}
			runner.removeSession(key, session)
			return
		}
		session.pendingDownload.Add(1)
		if session.downloadLimiter != nil {
			if err := session.downloadLimiter.wait(session.ctx, n); err != nil {
				session.pendingDownload.Add(-1)
				runner.removeSession(key, session)
				return
			}
		}
		if err := runner.writeToClient(buf[:n], session.clientAddr); err != nil {
			session.pendingDownload.Add(-1)
			runner.removeSession(key, session)
			return
		}
		runner.stats.addDownload(int64(n))
		session.lastSeen.Store(time.Now().UnixNano())
		session.pendingDownload.Add(-1)
	}
}

func refreshUDPReadDeadline(session *udpSession, now time.Time) error {
	if session.readUntil.Sub(now) > udpReadTimeout/2 {
		return nil
	}
	deadline := now.Add(udpReadTimeout)
	if err := session.targetConn.SetReadDeadline(deadline); err != nil {
		return err
	}
	session.readUntil = deadline
	return nil
}

func refreshUDPWriteDeadline(session *udpSession, now time.Time) error {
	if session.writeUntil.Sub(now) > udpWriteTimeout/2 {
		return nil
	}
	deadline := now.Add(udpWriteTimeout)
	if err := session.targetConn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	session.writeUntil = deadline
	return nil
}

func (runner *udpRunner) writeToClient(payload []byte, clientAddr netip.AddrPort) error {
	runner.clientWriteMu.Lock()
	defer runner.clientWriteMu.Unlock()
	now := time.Now()
	if runner.clientWriteUntil.Sub(now) <= udpWriteTimeout/2 {
		runner.clientWriteUntil = now.Add(udpWriteTimeout)
		if err := runner.ln.SetWriteDeadline(runner.clientWriteUntil); err != nil {
			return err
		}
	}
	_, err := runner.ln.WriteToUDPAddrPort(payload, clientAddr)
	return err
}

func (runner *udpRunner) cleanupLoop() {
	defer runner.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-runner.ctx.Done():
			return
		case <-ticker.C:
			runner.cleanupIdleSessions(60 * time.Second)
		}
	}
}

func (runner *udpRunner) cleanupIdleSessions(ttl time.Duration) {
	deadline := time.Now().Add(-ttl).UnixNano()
	runner.mu.Lock()
	for key, session := range runner.sessions {
		if session.queuedBytes.Load() > 0 || session.pendingDownload.Load() > 0 {
			continue
		}
		if session.lastSeen.Load() < deadline {
			runner.closeSessionLocked(key, session)
		}
	}
	runner.mu.Unlock()
}

func (runner *udpRunner) removeSession(key netip.AddrPort, expected *udpSession) {
	runner.mu.Lock()
	session, ok := runner.sessions[key]
	if ok && session == expected {
		runner.closeSessionLocked(key, session)
	}
	runner.mu.Unlock()
}

func (runner *udpRunner) closeSessionLocked(key netip.AddrPort, session *udpSession) {
	current, ok := runner.sessions[key]
	if !ok || current != session || !session.closed.CompareAndSwap(false, true) {
		return
	}
	delete(runner.sessions, key)
	session.cancel()
	_ = session.targetConn.Close()
	session.budget.release()
	runner.stats.decConns()
}
