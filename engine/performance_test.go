package engine

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

func BenchmarkTCPForwardingPersistent(b *testing.B) {
	targetAddr, stopTarget := benchmarkTCPEchoServer(b)
	defer stopTarget()

	b.Run("direct", func(b *testing.B) {
		conn, err := net.Dial("tcp", targetAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		benchmarkTCPRoundTrip(b, conn)
	})

	forwardAddr, stopForward := benchmarkTCPRunner(b, targetAddr)
	defer stopForward()
	b.Run("forwarded", func(b *testing.B) {
		conn, err := net.Dial("tcp", forwardAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		benchmarkTCPRoundTrip(b, conn)
	})
}

func BenchmarkTCPForwardingShortConnection(b *testing.B) {
	targetAddr, stopTarget := benchmarkTCPEchoServer(b)
	defer stopTarget()

	b.Run("direct", func(b *testing.B) {
		benchmarkTCPConnectRoundTrip(b, targetAddr)
	})

	forwardAddr, stopForward := benchmarkTCPRunner(b, targetAddr)
	defer stopForward()
	b.Run("forwarded", func(b *testing.B) {
		benchmarkTCPConnectRoundTrip(b, forwardAddr)
	})
}

func BenchmarkUDPForwardingRoundTrip(b *testing.B) {
	targetAddr, stopTarget := benchmarkUDPEchoServer(b)
	defer stopTarget()

	rule := Rule{
		RuleID: "udp-benchmark", Name: "udp-benchmark", Protocol: ProtocolUDP, Enabled: true,
		ListenAddr: "127.0.0.1", ListenPort: 0,
		TargetAddr: targetAddr.IP.String(), TargetPort: targetAddr.Port,
	}
	runner := newUDPRunner(rule, NewCollector()).(*udpRunner)
	if err := runner.Start(); err != nil {
		b.Fatal(err)
	}
	defer runner.Stop()
	forwardAddr := runner.ln.LocalAddr().(*net.UDPAddr)

	for _, size := range []int{64, 1400} {
		b.Run("direct/"+strconv.Itoa(size), func(b *testing.B) {
			benchmarkUDPRoundTrip(b, targetAddr, size)
		})
		b.Run("forwarded/"+strconv.Itoa(size), func(b *testing.B) {
			benchmarkUDPRoundTrip(b, forwardAddr, size)
		})
	}
}

func benchmarkTCPRunner(b *testing.B, targetAddr string) (string, func()) {
	b.Helper()
	host, portValue, err := net.SplitHostPort(targetAddr)
	if err != nil {
		b.Fatal(err)
	}
	port, err := strconv.Atoi(portValue)
	if err != nil {
		b.Fatal(err)
	}
	runner := newTCPRunner(Rule{
		RuleID: "tcp-benchmark", Name: "tcp-benchmark", Protocol: ProtocolTCP, Enabled: true,
		ListenAddr: "127.0.0.1", ListenPort: 0,
		TargetAddr: host, TargetPort: port, IdleTimeout: 30,
	}, NewCollector()).(*tcpRunner)
	if err := runner.Start(); err != nil {
		b.Fatal(err)
	}
	return runner.ln.Addr().String(), runner.Stop
}

func benchmarkTCPRoundTrip(b *testing.B, conn net.Conn) {
	payload := bytes.Repeat([]byte("x"), 256*1024)
	reply := make([]byte, len(payload))
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	b.ReportAllocs()
	b.SetBytes(int64(2 * len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkTCPConnectRoundTrip(b *testing.B, addr string) {
	payload := []byte("x")
	reply := make([]byte, len(payload))
	b.ReportAllocs()
	b.SetBytes(int64(2 * len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			b.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(payload); err != nil {
			_ = conn.Close()
			b.Fatal(err)
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			_ = conn.Close()
			b.Fatal(err)
		}
		_ = conn.Close()
	}
}

func benchmarkTCPEchoServer(b *testing.B) (string, func()) {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	var once sync.Once
	return listener.Addr().String(), func() {
		once.Do(func() {
			_ = listener.Close()
			<-done
		})
	}
}

func benchmarkUDPRoundTrip(b *testing.B, addr *net.UDPAddr, size int) {
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("u"), size)
	reply := make([]byte, size)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	b.ReportAllocs()
	b.SetBytes(int64(size * 2))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(payload); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkUDPEchoServer(b *testing.B) (*net.UDPAddr, func()) {
	b.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		b.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteToUDP(buf[:n], peer); err != nil {
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
