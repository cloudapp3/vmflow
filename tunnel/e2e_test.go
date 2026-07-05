package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/config"
)

func TestTunnelTCPForwarding(t *testing.T) {
	localLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skipIfSocketNotPermitted(t, err)
		t.Fatalf("listen local echo: %v", err)
	}
	defer localLn.Close()
	go runEchoServer(localLn)

	serverPort := freeTCPPort(t)
	remotePort := freeTCPPort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)
	localHost, localPort, err := net.SplitHostPort(localLn.Addr().String())
	if err != nil {
		t.Fatalf("split local echo addr: %v", err)
	}
	localPortInt := parsePortForTest(t, localPort)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	serverCfg := ServerConfig{
		Version: 1,
		Log:     config.LogConfig{Level: "error", Format: "text"},
		TunnelServer: TunnelServerConfig{
			ListenAddr:  serverAddr,
			OpenTimeout: "3s",
			Clients: []ServerClientACL{{
				ClientID: "home-01",
				Token:    "secret",
				Allow: AllowConfig{
					Protocols:   []string{"tcp"},
					RemotePorts: []int{remotePort},
				},
			}},
		},
	}
	clientCfg := ClientConfig{
		Version: 1,
		Log:     config.LogConfig{Level: "error", Format: "text"},
		TunnelClient: TunnelClientConfig{
			ServerAddr:   serverAddr,
			ClientID:     "home-01",
			Token:        "secret",
			DialTimeout:  "1s",
			ReconnectMin: "50ms",
			ReconnectMax: "100ms",
			Tunnels: []TunnelSpec{{
				TunnelID:         "echo",
				Protocol:         "tcp",
				RemoteListenAddr: "127.0.0.1",
				RemoteListenPort: remotePort,
				LocalAddr:        localHost,
				LocalPort:        localPortInt,
			}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(serverCfg, logger)
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Run(ctx) }()
	waitForTCP(t, serverAddr, 2*time.Second)

	clientErr := make(chan error, 1)
	go func() { clientErr <- NewClient(clientCfg, logger).Run(ctx) }()
	waitForTunnelCount(t, server, 1, 3*time.Second)
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", remotePort)
	waitForTCP(t, remoteAddr, 3*time.Second)

	conn, err := net.DialTimeout("tcp", remoteAddr, time.Second)
	if err != nil {
		t.Fatalf("dial remote tunnel: %v", err)
	}
	if _, err := conn.Write([]byte("hello vmflow tunnel")); err != nil {
		t.Fatalf("write tunnel: %v", err)
	}
	buf := make([]byte, len("hello vmflow tunnel"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read tunnel echo: %v", err)
	}
	if string(buf) != "hello vmflow tunnel" {
		_ = conn.Close()
		t.Fatalf("echo = %q", string(buf))
	}
	_ = conn.Close()

	cancel()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not stop")
	}
	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("client Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("client did not stop")
	}
}

func runEchoServer(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}()
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skipIfSocketNotPermitted(t, err)
		t.Fatalf("allocate tcp port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split free port: %v", err)
	}
	return parsePortForTest(t, port)
}

func parsePortForTest(t *testing.T, value string) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(value, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", value, err)
	}
	return port
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", addr, lastErr)
}

func writeTempConfig(t *testing.T, raw []byte) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "vmflow-tunnel-*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}
	return file.Name()
}

func TestTunnelUDPForwarding(t *testing.T) {
	localAddr, closeLocal := startUDPEchoServer(t)
	defer closeLocal()

	serverPort := freeTCPPort(t)
	remotePort := freeUDPPort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)
	localHost, localPort, err := net.SplitHostPort(localAddr.String())
	if err != nil {
		t.Fatalf("split local udp addr: %v", err)
	}
	localPortInt := parsePortForTest(t, localPort)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	serverCfg := ServerConfig{
		Version: 1,
		Log:     config.LogConfig{Level: "error", Format: "text"},
		TunnelServer: TunnelServerConfig{
			ListenAddr:  serverAddr,
			OpenTimeout: "3s",
			Clients: []ServerClientACL{{
				ClientID: "home-01",
				Token:    "secret",
				Allow: AllowConfig{
					Protocols:   []string{"udp"},
					RemotePorts: []int{remotePort},
				},
			}},
		},
	}
	clientCfg := ClientConfig{
		Version: 1,
		Log:     config.LogConfig{Level: "error", Format: "text"},
		TunnelClient: TunnelClientConfig{
			ServerAddr:   serverAddr,
			ClientID:     "home-01",
			Token:        "secret",
			DialTimeout:  "1s",
			ReconnectMin: "50ms",
			ReconnectMax: "100ms",
			Tunnels: []TunnelSpec{{
				TunnelID:         "udp-echo",
				Protocol:         "udp",
				RemoteListenAddr: "127.0.0.1",
				RemoteListenPort: remotePort,
				LocalAddr:        localHost,
				LocalPort:        localPortInt,
			}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(serverCfg, logger)
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Run(ctx) }()
	waitForTCP(t, serverAddr, 2*time.Second)

	clientErr := make(chan error, 1)
	go func() { clientErr <- NewClient(clientCfg, logger).Run(ctx) }()
	waitForTunnelCount(t, server, 1, 3*time.Second)

	conn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", remotePort), time.Second)
	if err != nil {
		t.Fatalf("dial remote udp tunnel: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set udp deadline: %v", err)
	}
	if _, err := conn.Write([]byte("hello udp tunnel")); err != nil {
		t.Fatalf("write udp tunnel: %v", err)
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read udp tunnel echo: %v", err)
	}
	if string(buf[:n]) != "hello udp tunnel" {
		t.Fatalf("udp echo = %q", string(buf[:n]))
	}
	_ = conn.Close()

	cancel()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not stop")
	}
	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("client Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("client did not stop")
	}
}

func startUDPEchoServer(t *testing.T) (*net.UDPAddr, func()) {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp echo addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		skipIfSocketNotPermitted(t, err)
		t.Fatalf("listen udp echo: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			n, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], remote)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr), func() {
		_ = conn.Close()
		<-done
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp free port: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		skipIfSocketNotPermitted(t, err)
		t.Fatalf("allocate udp port: %v", err)
	}
	defer conn.Close()
	_, port, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("split free udp port: %v", err)
	}
	return parsePortForTest(t, port)
}

func waitForTunnelCount(t *testing.T, server *Server, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if server.RunningTunnels() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d tunnel(s), got %d", want, server.RunningTunnels())
}

func skipIfSocketNotPermitted(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
		t.Skipf("local sockets are not permitted in this sandbox: %v", err)
	}
}
