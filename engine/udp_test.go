package engine

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestUDPDefaultRuleSessionLimitRejectsOverflow(t *testing.T) {
	collector := NewCollector()
	runner := newUDPRunner(Rule{RuleID: "udp-default-limit"}, collector).(*udpRunner)
	defer runner.cancel()

	if got := runner.sessionLimit(); got != DefaultUDPRuleMaxSessions {
		t.Fatalf("session limit = %d, want %d", got, DefaultUDPRuleMaxSessions)
	}
	for port := 1; port <= DefaultUDPRuleMaxSessions; port++ {
		key := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
		runner.sessions[key] = &udpSession{}
	}

	overflowKey := netip.MustParseAddrPort("127.0.0.1:65000")
	runner.handlePacket(overflowKey, []byte("drop"))
	if got := len(runner.sessions); got != DefaultUDPRuleMaxSessions {
		t.Fatalf("sessions = %d, want %d", got, DefaultUDPRuleMaxSessions)
	}
	if got := collector.Snapshot("udp-default-limit").UDPSessionRejected; got != 1 {
		t.Fatalf("rejected sessions = %d, want 1", got)
	}
	_, active := runner.budget.snapshot()
	if active != 0 {
		t.Fatalf("budget active = %d, want 0", active)
	}
}

func TestUDPRunnerBudgetPairsSessionCreateAndClose(t *testing.T) {
	target, stopTarget := startUDPSink(t)
	defer stopTarget()

	budget := newUDPSessionBudget(1)
	runner := newUDPRunnerWithBudget(
		Rule{RuleID: "udp-budget", MaxConn: 10},
		NewCollector(),
		budget,
	).(*udpRunner)
	runner.target = target

	firstKey := netip.MustParseAddrPort("127.0.0.1:12001")
	secondKey := netip.MustParseAddrPort("127.0.0.1:12002")
	runner.handlePacket(firstKey, []byte("first"))
	runner.handlePacket(secondKey, []byte("second"))

	runner.mu.Lock()
	if got := len(runner.sessions); got != 1 {
		runner.mu.Unlock()
		runner.Stop()
		t.Fatalf("sessions = %d, want 1", got)
	}
	first := runner.sessions[firstKey]
	runner.mu.Unlock()
	if first == nil {
		runner.Stop()
		t.Fatal("first session was not admitted")
	}
	if _, active := budget.snapshot(); active != 1 {
		runner.Stop()
		t.Fatalf("budget active = %d, want 1", active)
	}

	runner.removeSession(firstKey, first)
	if _, active := budget.snapshot(); active != 0 {
		runner.Stop()
		t.Fatalf("budget active after close = %d, want 0", active)
	}
	runner.handlePacket(secondKey, []byte("second"))
	if _, active := budget.snapshot(); active != 1 {
		runner.Stop()
		t.Fatalf("budget active after reuse = %d, want 1", active)
	}

	runner.Stop()
	if _, active := budget.snapshot(); active != 0 {
		t.Fatalf("budget active after stop = %d, want 0", active)
	}
}

func TestUDPRemoveSessionRequiresExpectedGeneration(t *testing.T) {
	target, stopTarget := startUDPSink(t)
	defer stopTarget()

	budget := newUDPSessionBudget(2)
	runner := newUDPRunnerWithBudget(
		Rule{RuleID: "udp-generation"},
		NewCollector(),
		budget,
	).(*udpRunner)
	runner.target = target
	key := netip.MustParseAddrPort("127.0.0.1:13001")

	runner.handlePacket(key, []byte("old"))
	old := udpSessionForKey(t, runner, key)
	runner.removeSession(key, old)
	runner.handlePacket(key, []byte("new"))
	current := udpSessionForKey(t, runner, key)
	if current == old {
		runner.Stop()
		t.Fatal("expected a new session generation")
	}

	runner.removeSession(key, old)
	if got := udpSessionForKey(t, runner, key); got != current {
		runner.Stop()
		t.Fatal("stale cleanup removed the current session")
	}
	if _, active := budget.snapshot(); active != 1 {
		runner.Stop()
		t.Fatalf("budget active = %d, want 1", active)
	}

	runner.Stop()
}

func TestUDPCleanupPreservesSessionsWithPendingTraffic(t *testing.T) {
	target, stopTarget := startUDPSink(t)
	defer stopTarget()

	budget := newUDPSessionBudget(1)
	runner := newUDPRunnerWithBudget(
		Rule{RuleID: "udp-pending-cleanup"},
		NewCollector(),
		budget,
	).(*udpRunner)
	runner.target = target
	defer runner.Stop()

	key := netip.MustParseAddrPort("127.0.0.1:13201")
	runner.handlePacket(key, []byte("create"))
	session := udpSessionForKey(t, runner, key)
	ttl := time.Second
	markIdle := func() {
		session.lastSeen.Store(time.Now().Add(-2 * ttl).UnixNano())
	}

	markIdle()
	session.queuedBytes.Store(1)
	runner.cleanupIdleSessions(ttl)
	if got := udpSessionForKey(t, runner, key); got != session {
		t.Fatal("cleanup removed a session with queued upload data")
	}
	session.queuedBytes.Store(0)

	markIdle()
	session.pendingDownload.Store(1)
	runner.cleanupIdleSessions(ttl)
	if got := udpSessionForKey(t, runner, key); got != session {
		t.Fatal("cleanup removed a session with a pending download")
	}
	session.pendingDownload.Store(0)

	markIdle()
	runner.cleanupIdleSessions(ttl)
	runner.mu.Lock()
	_, exists := runner.sessions[key]
	runner.mu.Unlock()
	if exists {
		t.Fatal("cleanup kept an idle session after pending traffic cleared")
	}
	if _, active := budget.snapshot(); active != 0 {
		t.Fatalf("budget active after cleanup = %d, want 0", active)
	}
}

func TestTCPUDPRunnerStopPreservesSharedConnectionCount(t *testing.T) {
	target, stopTarget := startUDPSink(t)
	defer stopTarget()
	collector := NewCollector()
	udp := newUDPRunner(Rule{RuleID: "combined"}, collector).(*udpRunner)
	udp.target = target
	key := netip.MustParseAddrPort("127.0.0.1:13501")
	udp.handlePacket(key, []byte("active"))
	if got := collector.Snapshot("combined").Conns; got != 1 {
		udp.Stop()
		t.Fatalf("connections = %d before TCP stop, want 1", got)
	}

	tcp := newTCPRunner(Rule{RuleID: "combined"}, collector).(*tcpRunner)
	tcp.Stop()
	if got := collector.Snapshot("combined").Conns; got != 1 {
		udp.Stop()
		t.Fatalf("TCP stop reset shared UDP connection count to %d", got)
	}
	udp.Stop()
	if got := collector.Snapshot("combined").Conns; got != 0 {
		t.Fatalf("connections = %d after both runners stopped, want 0", got)
	}
}

func TestUDPSpeedLimitDoesNotBlockOtherSessions(t *testing.T) {
	target, packets, stopTarget := startUDPRecorder(t)
	defer stopTarget()

	rule := udpTestRule("udp-limit-isolation", target)
	rule.SpeedLimit = 1
	runner := newUDPRunner(rule, NewCollector()).(*udpRunner)
	if err := runner.Start(); err != nil {
		t.Fatalf("start UDP runner: %v", err)
	}
	defer runner.Stop()

	first := listenUDPClient(t)
	defer first.Close()
	second := listenUDPClient(t)
	defer second.Close()
	forwardAddr := runner.ln.LocalAddr().(*net.UDPAddr).AddrPort()

	if _, err := first.WriteToUDPAddrPort([]byte("AA"), forwardAddr); err != nil {
		t.Fatalf("write slow packet: %v", err)
	}
	waitForUDPSessionCount(t, runner, 1)
	if _, err := second.WriteToUDPAddrPort([]byte("B"), forwardAddr); err != nil {
		t.Fatalf("write independent packet: %v", err)
	}

	select {
	case got := <-packets:
		if string(got) != "B" {
			t.Fatalf("first forwarded packet = %q, want independent packet B", got)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("a throttled session blocked another UDP session")
	}
}

func TestUDPLimitedUploadQueueDropsWhenFull(t *testing.T) {
	collector := NewCollector()
	runner := newUDPRunner(Rule{RuleID: "udp-queue-full"}, collector).(*udpRunner)
	defer runner.cancel()
	sessionCtx, sessionCancel := context.WithCancel(runner.ctx)
	defer sessionCancel()
	session := &udpSession{
		ctx:         sessionCtx,
		uploadQueue: make(chan *udpQueuedPacket, 1),
	}
	queued := acquireUDPQueuedPacket([]byte("already queued"))
	session.uploadQueue <- queued
	defer func() { releaseUDPQueuedPacket(<-session.uploadQueue) }()
	session.queuedBytes.Store(queued.accountedBytes)

	runner.enqueueUpload(session, []byte("drop"))
	if got := collector.Snapshot("udp-queue-full").UDPPacketsDropped; got != 1 {
		t.Fatalf("dropped packets = %d, want 1", got)
	}
	if got := session.queuedBytes.Load(); got != queued.accountedBytes {
		t.Fatalf("queued bytes = %d after drop", got)
	}
}

func TestUDPLimitedUploadQueueReusesPacketBuffers(t *testing.T) {
	runner := newUDPRunner(Rule{RuleID: "udp-queue-pool"}, NewCollector()).(*udpRunner)
	defer runner.cancel()
	sessionCtx, sessionCancel := context.WithCancel(runner.ctx)
	defer sessionCancel()
	session := &udpSession{
		ctx:         sessionCtx,
		uploadQueue: make(chan *udpQueuedPacket, 1),
	}
	payload := make([]byte, 1400)

	runner.enqueueUpload(session, payload)
	packet := <-session.uploadQueue
	session.queuedBytes.Add(-packet.accountedBytes)
	releaseUDPQueuedPacket(packet)

	allocs := testing.AllocsPerRun(1000, func() {
		runner.enqueueUpload(session, payload)
		packet := <-session.uploadQueue
		session.queuedBytes.Add(-packet.accountedBytes)
		releaseUDPQueuedPacket(packet)
	})
	if allocs != 0 {
		t.Fatalf("limited UDP queue allocated %.2f objects per packet", allocs)
	}
}

func TestUDPLimitedUploadQueueAccountsForPooledCapacity(t *testing.T) {
	collector := NewCollector()
	runner := newUDPRunner(Rule{RuleID: "udp-queue-capacity"}, collector).(*udpRunner)
	defer runner.cancel()
	sessionCtx, sessionCancel := context.WithCancel(runner.ctx)
	defer sessionCancel()
	session := &udpSession{
		ctx:         sessionCtx,
		uploadQueue: make(chan *udpQueuedPacket, udpUploadQueueCapacity),
	}
	payload := make([]byte, 1025)

	for range 17 {
		runner.enqueueUpload(session, payload)
	}
	if got := len(session.uploadQueue); got != 16 {
		t.Fatalf("queued packets = %d, want 16 buffers within 64 KiB", got)
	}
	if got := session.queuedBytes.Load(); got != udpUploadQueueMaxBytes {
		t.Fatalf("accounted queue bytes = %d, want %d", got, udpUploadQueueMaxBytes)
	}
	if got := collector.Snapshot("udp-queue-capacity").UDPPacketsDropped; got != 1 {
		t.Fatalf("dropped packets = %d, want 1", got)
	}
	runner.drainUploadQueue(session)
	if got := session.queuedBytes.Load(); got != 0 {
		t.Fatalf("accounted queue bytes after drain = %d, want 0", got)
	}
}

func TestUDPLimitedUploadRefreshesActivityBeforeClearingPending(t *testing.T) {
	target, packets, stopTarget := startUDPRecorder(t)
	defer stopTarget()

	rule := udpTestRule("udp-upload-activity", target)
	rule.SpeedLimit = 1024
	runner := newUDPRunner(rule, NewCollector()).(*udpRunner)
	runner.target = target
	defer runner.Stop()

	key := netip.MustParseAddrPort("127.0.0.1:14201")
	runner.handlePacket(key, []byte("create"))
	session := udpSessionForKey(t, runner, key)
	select {
	case <-packets:
	case <-time.After(2 * time.Second):
		t.Fatal("initial packet was not forwarded")
	}
	waitForUDPQueuedBytes(t, session, 0)

	oldActivity := time.Now().Add(-2 * time.Minute).UnixNano()
	session.lastSeen.Store(oldActivity)
	runner.enqueueUpload(session, []byte("refresh"))
	select {
	case <-packets:
	case <-time.After(2 * time.Second):
		t.Fatal("queued packet was not forwarded")
	}
	waitForUDPQueuedBytes(t, session, 0)
	if got := session.lastSeen.Load(); got <= oldActivity {
		t.Fatalf("last activity = %d, want newer than %d", got, oldActivity)
	}
}

func TestUDPUnlimitedBasicForwarding(t *testing.T) {
	target, stopTarget := startUDPEchoTarget(t)
	defer stopTarget()

	rule := udpTestRule("udp-echo", target)
	collector := NewCollector()
	runner := newUDPRunner(rule, collector).(*udpRunner)
	if err := runner.Start(); err != nil {
		t.Fatalf("start UDP runner: %v", err)
	}
	defer runner.Stop()

	client := listenUDPClient(t)
	defer client.Close()
	payload := []byte("hello-vmflow-udp")
	if _, err := client.WriteToUDPAddrPort(payload, runner.ln.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatalf("write to runner: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, _, err := client.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("read echoed payload: %v", err)
	}
	if got := string(buf[:n]); got != string(payload) {
		t.Fatalf("echoed payload = %q, want %q", got, payload)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot := collector.Snapshot(rule.RuleID)
		if snapshot.UploadBytes == int64(len(payload)) && snapshot.DownloadBytes == int64(len(payload)) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("traffic snapshot = %+v", collector.Snapshot(rule.RuleID))
}

func TestUDPForwardsZeroLengthDatagram(t *testing.T) {
	target, stopTarget := startUDPEchoTarget(t)
	defer stopTarget()

	rule := udpTestRule("udp-empty-datagram", target)
	runner := newUDPRunner(rule, NewCollector()).(*udpRunner)
	if err := runner.Start(); err != nil {
		t.Fatalf("start UDP runner: %v", err)
	}
	defer runner.Stop()

	client := listenUDPClient(t)
	defer client.Close()
	forwardAddr := runner.ln.LocalAddr().(*net.UDPAddr).AddrPort()
	if n, err := client.WriteToUDPAddrPort(nil, forwardAddr); err != nil || n != 0 {
		t.Fatalf("write empty datagram = (%d, %v), want (0, nil)", n, err)
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1)
	n, _, err := client.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("read empty echoed datagram: %v", err)
	}
	if n != 0 {
		t.Fatalf("echoed datagram length = %d, want 0", n)
	}
}

func TestUDPUnlimitedHotPathDoesNotAllocate(t *testing.T) {
	target, stopTarget := startUDPSink(t)
	defer stopTarget()

	runner := newUDPRunnerWithBudget(
		Rule{RuleID: "udp-no-limit-alloc"},
		NewCollector(),
		newUDPSessionBudget(2),
	).(*udpRunner)
	runner.target = target
	defer runner.Stop()

	key := netip.MustParseAddrPort("127.0.0.1:14001")
	payload := []byte("allocation-check")
	runner.handlePacket(key, payload)
	if session := udpSessionForKey(t, runner, key); session.uploadQueue != nil {
		t.Fatal("unlimited session unexpectedly created an upload queue")
	}

	allocs := testing.AllocsPerRun(1000, func() {
		runner.handlePacket(key, payload)
	})
	if allocs != 0 {
		t.Fatalf("unlimited UDP hot path allocations = %.2f, want 0", allocs)
	}
}

func udpTestRule(ruleID string, target *net.UDPAddr) Rule {
	return Rule{
		RuleID:     ruleID,
		Name:       ruleID,
		Protocol:   ProtocolUDP,
		ListenAddr: "127.0.0.1",
		ListenPort: 0,
		TargetAddr: target.IP.String(),
		TargetPort: target.Port,
		Enabled:    true,
	}
}

func listenUDPClient(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP client: %v", err)
	}
	return conn
}

func startUDPSink(t *testing.T) (*net.UDPAddr, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP sink: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			if _, _, err := conn.ReadFromUDPAddrPort(buf); err != nil {
				return
			}
		}
	}()
	var once sync.Once
	return conn.LocalAddr().(*net.UDPAddr), func() {
		once.Do(func() {
			_ = conn.Close()
			<-done
		})
	}
}

func startUDPRecorder(t *testing.T) (*net.UDPAddr, <-chan []byte, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP recorder: %v", err)
	}
	packets := make(chan []byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			n, _, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			packet := append([]byte(nil), buf[:n]...)
			select {
			case packets <- packet:
			default:
			}
		}
	}()
	var once sync.Once
	return conn.LocalAddr().(*net.UDPAddr), packets, func() {
		once.Do(func() {
			_ = conn.Close()
			<-done
		})
	}
}

func startUDPEchoTarget(t *testing.T) (*net.UDPAddr, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP echo target: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteToUDPAddrPort(buf[:n], addr); err != nil {
				return
			}
		}
	}()
	var once sync.Once
	return conn.LocalAddr().(*net.UDPAddr), func() {
		once.Do(func() {
			_ = conn.Close()
			<-done
		})
	}
}

func udpSessionForKey(t *testing.T, runner *udpRunner, key netip.AddrPort) *udpSession {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	session := runner.sessions[key]
	if session == nil {
		t.Fatalf("missing UDP session for %s", key)
	}
	return session
}

func waitForUDPSessionCount(t *testing.T, runner *udpRunner, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		got := len(runner.sessions)
		runner.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("UDP session count did not reach %d", want)
}

func waitForUDPQueuedBytes(t *testing.T, session *udpSession, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := session.queuedBytes.Load(); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("UDP queued bytes did not reach %d; got %d", want, session.queuedBytes.Load())
}
