package engine

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type udpRunner struct {
	rule      Rule
	collector *Collector
	target    *net.UDPAddr

	ctx    context.Context
	cancel context.CancelFunc
	ln     *net.UDPConn

	mu       sync.Mutex
	sessions map[string]*udpSession
	wg       sync.WaitGroup
}

type udpSession struct {
	targetConn *net.UDPConn
	clientAddr *net.UDPAddr
	lastSeen   atomic.Int64
	closed     atomic.Bool

	uploadLimiter   *rateLimiter
	downloadLimiter *rateLimiter
}

func newUDPRunner(rule Rule, collector *Collector) Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &udpRunner{
		rule:      rule,
		collector: collector,
		ctx:       ctx,
		cancel:    cancel,
		sessions:  make(map[string]*udpSession),
	}
}

func (runner *udpRunner) Start() error {
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
	runner.collector.SetConns(runner.rule.RuleID, 0)
}

func (runner *udpRunner) readLoop() {
	defer runner.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, clientAddr, err := runner.ln.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-runner.ctx.Done():
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		runner.handlePacket(clientAddr, payload)
	}
}

func (runner *udpRunner) handlePacket(clientAddr *net.UDPAddr, payload []byte) {
	if clientAddr == nil || len(payload) == 0 {
		return
	}
	key := clientAddr.String()
	now := time.Now().UnixNano()

	runner.mu.Lock()
	session, ok := runner.sessions[key]
	if !ok {
		if runner.rule.MaxConn > 0 && len(runner.sessions) >= runner.rule.MaxConn {
			runner.mu.Unlock()
			return
		}
		targetConn, err := net.DialUDP("udp", nil, runner.target)
		if err != nil {
			runner.mu.Unlock()
			return
		}
		session = &udpSession{targetConn: targetConn, clientAddr: clientAddr}
		if runner.rule.SpeedLimit > 0 {
			session.uploadLimiter = newRateLimiter(runner.rule.SpeedLimit)
			session.downloadLimiter = newRateLimiter(runner.rule.SpeedLimit)
		}
		session.lastSeen.Store(now)
		runner.sessions[key] = session
		runner.collector.IncConns(runner.rule.RuleID)
		runner.wg.Add(1)
		go runner.readFromTarget(key, session)
	}
	session.lastSeen.Store(now)
	runner.mu.Unlock()

	if session.uploadLimiter != nil {
		session.uploadLimiter.wait(len(payload))
	}
	if _, err := session.targetConn.Write(payload); err != nil {
		runner.removeSession(key)
		return
	}
	runner.collector.AddUpload(runner.rule.RuleID, int64(len(payload)))
}

func (runner *udpRunner) readFromTarget(key string, session *udpSession) {
	defer runner.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		if err := session.targetConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			runner.removeSession(key)
			return
		}
		n, err := session.targetConn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				select {
				case <-runner.ctx.Done():
					runner.removeSession(key)
					return
				default:
					continue
				}
			}
			runner.removeSession(key)
			return
		}
		if session.downloadLimiter != nil {
			session.downloadLimiter.wait(n)
		}
		if _, err := runner.ln.WriteToUDP(buf[:n], session.clientAddr); err != nil {
			runner.removeSession(key)
			return
		}
		runner.collector.AddDownload(runner.rule.RuleID, int64(n))
		session.lastSeen.Store(time.Now().UnixNano())
	}
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
		if session.lastSeen.Load() < deadline {
			runner.closeSessionLocked(key, session)
		}
	}
	runner.mu.Unlock()
}

func (runner *udpRunner) removeSession(key string) {
	runner.mu.Lock()
	session, ok := runner.sessions[key]
	if ok {
		runner.closeSessionLocked(key, session)
	}
	runner.mu.Unlock()
}

func (runner *udpRunner) closeSessionLocked(key string, session *udpSession) {
	delete(runner.sessions, key)
	if session.closed.CompareAndSwap(false, true) {
		_ = session.targetConn.Close()
		runner.collector.DecConns(runner.rule.RuleID)
	}
}
