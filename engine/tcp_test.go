package engine

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

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
