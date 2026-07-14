package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiterWaitCanBeCancelled(t *testing.T) {
	limiter := newRateLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- limiter.wait(ctx, 32*1024)
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rate limiter wait did not stop after cancellation")
	}
}

// startHoldingTarget listens on 127.0.0.1:0 and accepts connections, holding
// each open without sending or receiving data (simulates a server that keeps
// the socket alive while idle). Returns its address and a stop function.
func startHoldingTarget(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open until it is closed underneath us.
			go func(c net.Conn) { _, _ = io.Copy(io.Discard, c); _ = c.Close() }(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func mustSplitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return host, p
}

func waitForTCPConns(t *testing.T, collector *Collector, ruleID string, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if collector.Snapshot(ruleID).Conns == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connections = %d, want %d", collector.Snapshot(ruleID).Conns, want)
}

func TestTCPMaxConnBurstNeverExceedsLimit(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()
	var targetAccepted atomic.Int64
	go func() {
		for {
			conn, err := targetLn.Accept()
			if err != nil {
				return
			}
			targetAccepted.Add(1)
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()
	targetHost, targetPort := mustSplitHostPort(t, targetLn.Addr().String())

	rule := Rule{
		RuleID: "max-conn-burst", Name: "max-conn-burst", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: targetHost, TargetPort: targetPort,
		Enabled: true, MaxConn: 1,
	}
	collector := NewCollector()
	runner := newTCPRunner(rule, collector)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer runner.Stop()
	tr := runner.(*tcpRunner)

	const attempts = 64
	start := make(chan struct{})
	results := make(chan net.Conn, attempts)
	for range attempts {
		go func() {
			<-start
			conn, _ := net.DialTimeout("tcp", tr.ln.Addr().String(), 2*time.Second)
			results <- conn
		}()
	}
	close(start)
	clients := make([]net.Conn, 0, attempts)
	for range attempts {
		if conn := <-results; conn != nil {
			clients = append(clients, conn)
		}
	}
	defer func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
	}()

	waitForTCPConns(t, collector, rule.RuleID, 1, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for targetAccepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := targetAccepted.Load(); got != 1 {
		t.Fatalf("target accepted %d connections, want exactly 1", got)
	}
	if got := tr.activeConn.Load(); got != 1 {
		t.Fatalf("active connections = %d, want 1", got)
	}
}

func TestTCPSingleDirectionActivityKeepsReversePathAlive(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()
	targetConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := targetLn.Accept()
		if err == nil {
			targetConnCh <- conn
		}
	}()
	targetHost, targetPort := mustSplitHostPort(t, targetLn.Addr().String())

	rule := Rule{
		RuleID: "shared-idle", Name: "shared-idle", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: targetHost, TargetPort: targetPort,
		Enabled: true, IdleTimeout: 1,
	}
	collector := NewCollector()
	runner := newTCPRunner(rule, collector)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer runner.Stop()
	tr := runner.(*tcpRunner)

	client, err := net.Dial("tcp", tr.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var targetConn net.Conn
	select {
	case targetConn = <-targetConnCh:
		defer targetConn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("target connection was not established")
	}

	stopWrites := make(chan struct{})
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWrites:
				return
			case <-ticker.C:
				if _, err := client.Write([]byte{1}); err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(stopWrites)
		<-writeDone
	}()

	// Keep upload active beyond idle_timeout before the target sends anything.
	time.Sleep(1500 * time.Millisecond)
	reply := []byte("reverse-path-still-live")
	if _, err := targetConn.Write(reply); err != nil {
		t.Fatalf("target write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("reverse path did not recover after one-way activity: %v", err)
	}
	if string(got) != string(reply) {
		t.Fatalf("reply = %q, want %q", got, reply)
	}
}

func TestTCPPendingRateLimitedDataIsNotIdle(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()
	targetData := make(chan []byte, 1)
	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 3*1024)
		_, err = io.ReadFull(conn, buf)
		if err == nil {
			targetData <- buf
		}
	}()
	targetHost, targetPort := mustSplitHostPort(t, targetLn.Addr().String())

	rule := Rule{
		RuleID: "limited-not-idle", Name: "limited-not-idle", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: targetHost, TargetPort: targetPort,
		Enabled: true, IdleTimeout: 1, SpeedLimit: 1024,
	}
	runner := newTCPRunner(rule, NewCollector()).(*tcpRunner)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer runner.Stop()
	client, err := net.Dial("tcp", runner.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := make([]byte, 3*1024)
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-targetData:
		if len(got) != len(payload) {
			t.Fatalf("target received %d bytes, want %d", len(got), len(payload))
		}
	case <-time.After(4 * time.Second):
		t.Fatal("pending rate-limited data was closed by idle timeout")
	}
}

func TestTCPCopyWriteTimeout(t *testing.T) {
	src, srcPeer := net.Pipe()
	dst, dstPeer := net.Pipe()
	defer src.Close()
	defer srcPeer.Close()
	defer dst.Close()
	defer dstPeer.Close()

	done := make(chan error, 1)
	go func() {
		_, err := copyTCPDirection(context.Background(), dst, src, 0, 100*time.Millisecond, newTCPActivity(), nil)
		done <- err
	}()
	if _, err := srcPeer.Write([]byte("blocked-write")); err != nil {
		t.Fatalf("source write: %v", err)
	}
	select {
	case err := <-done:
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("copy error = %v, want a write timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked target write was not bounded")
	}
}

func TestTCPHalfCloseRelaysFinalResponse(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()
	request := []byte("request-before-fin")
	reply := []byte("response-after-fin")
	targetDone := make(chan error, 1)
	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			targetDone <- err
			return
		}
		defer conn.Close()
		got, err := io.ReadAll(conn)
		if err != nil {
			targetDone <- err
			return
		}
		if string(got) != string(request) {
			targetDone <- errors.New("target received an unexpected request")
			return
		}
		_, err = conn.Write(reply)
		targetDone <- err
	}()
	targetHost, targetPort := mustSplitHostPort(t, targetLn.Addr().String())

	rule := Rule{
		RuleID: "half-close", Name: "half-close", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: targetHost, TargetPort: targetPort,
		Enabled: true, IdleTimeout: 5,
	}
	runner := newTCPRunner(rule, NewCollector())
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer runner.Stop()
	tr := runner.(*tcpRunner)

	conn, err := net.DialTCP("tcp", nil, tr.ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read final response: %v", err)
	}
	if string(got) != string(reply) {
		t.Fatalf("reply = %q, want %q", got, reply)
	}
	if err := <-targetDone; err != nil {
		t.Fatalf("target: %v", err)
	}
}

// TestTCPStopUnblocksSilentClient asserts that Stop returns promptly even when a
// client has opened a connection and then gone silent (the exact condition that
// used to wedge Stop/reload/shutdown forever).
func TestTCPStopUnblocksSilentClient(t *testing.T) {
	addr, stopTarget := startHoldingTarget(t)
	defer stopTarget()
	host, port := mustSplitHostPort(t, addr)

	rule := Rule{RuleID: "stop-test", Name: "stop-test", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: host, TargetPort: port, Enabled: true}
	collector := NewCollector()
	runner := newTCPRunner(rule, collector)
	if err := runner.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	tr := runner.(*tcpRunner)
	listenAddr := tr.ln.Addr().String()

	client, err := net.Dial("tcp", listenAddr)
	if err != nil {
		runner.Stop()
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Wait for the daemon to register the active connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if collector.Snapshot(rule.RuleID).Conns > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if collector.Snapshot(rule.RuleID).Conns == 0 {
		runner.Stop()
		t.Fatal("connection never established")
	}

	// Stop must return within 2s; before the fix it blocked forever.
	done := make(chan struct{})
	go func() { runner.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on silent client connection")
	}
}

// TestTCPIdleTimeoutReapsSilentClient asserts that a silent connection is reaped
// after the idle timeout elapses.
func TestTCPIdleTimeoutReapsSilentClient(t *testing.T) {
	addr, stopTarget := startHoldingTarget(t)
	defer stopTarget()
	host, port := mustSplitHostPort(t, addr)

	rule := Rule{RuleID: "idle-test", Name: "idle-test", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: host, TargetPort: port,
		Enabled: true, IdleTimeout: 1}
	collector := NewCollector()
	runner := newTCPRunner(rule, collector)
	if err := runner.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runner.Stop()
	tr := runner.(*tcpRunner)

	client, err := net.Dial("tcp", tr.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	waitForTCPConns(t, collector, rule.RuleID, 1, 2*time.Second)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if collector.Snapshot(rule.RuleID).Conns == 0 {
			return // reaped
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("idle connection not reaped within idle timeout: conns=%d", collector.Snapshot(rule.RuleID).Conns)
}

// TestTCPCopyStillRelays asserts the idle-timeout path does not break normal
// bidirectional relay.
func TestTCPCopyStillRelays(t *testing.T) {
	// echo target
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()
	go func() {
		for {
			c, err := targetLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c) // echo
		}
	}()
	host, port := mustSplitHostPort(t, targetLn.Addr().String())

	rule := Rule{RuleID: "echo", Name: "echo", Protocol: ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: 0, TargetAddr: host, TargetPort: port,
		Enabled: true, IdleTimeout: 10}
	collector := NewCollector()
	runner := newTCPRunner(rule, collector)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer runner.Stop()
	tr := runner.(*tcpRunner)

	client, err := net.Dial("tcp", tr.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	msg := []byte("hello-vmflow\n")
	if _, err := client.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("relay read failed: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("relay mismatch: got %q want %q", buf, msg)
	}
}
