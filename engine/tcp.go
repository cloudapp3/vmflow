package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTCPIdleTimeout bounds how long a TCP forwarding connection may stay
// idle (no data in either direction) before being reaped. It keeps Stop/reload
// from hanging on stuck or silent clients and caps per-connection resource
// hold time. Overridable per rule via idle_timeout (seconds); 0 means use this
// default.
const DefaultTCPIdleTimeout = 5 * time.Minute

type tcpRunner struct {
	rule      Rule
	collector *Collector

	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener

	activeConn atomic.Int64
	wg         sync.WaitGroup
	connMu     sync.Mutex
	conns      map[net.Conn]struct{}
}

func newTCPRunner(rule Rule, collector *Collector) Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &tcpRunner{
		rule:      rule,
		collector: collector,
		ctx:       ctx,
		cancel:    cancel,
		conns:     make(map[net.Conn]struct{}),
	}
}

func (runner *tcpRunner) Start() error {
	listenAddr := net.JoinHostPort(strings.TrimSpace(runner.rule.ListenAddr), strconv.Itoa(runner.rule.ListenPort))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	runner.ln = listener
	runner.wg.Add(1)
	go runner.acceptLoop()
	return nil
}

func (runner *tcpRunner) Stop() {
	runner.cancel()
	if runner.ln != nil {
		_ = runner.ln.Close()
	}
	// Force-close every established client connection so blocked copy loops
	// unblock and handleConn returns; otherwise wg.Wait could hang on a silent
	// client forever. (DialContext below also cancels any in-flight dial.)
	runner.closeAllConns()
	runner.wg.Wait()
	runner.collector.SetConns(runner.rule.RuleID, 0)
}

func (runner *tcpRunner) acceptLoop() {
	defer runner.wg.Done()
	for {
		conn, err := runner.ln.Accept()
		if err != nil {
			select {
			case <-runner.ctx.Done():
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		if runner.rule.MaxConn > 0 && runner.activeConn.Load() >= int64(runner.rule.MaxConn) {
			_ = conn.Close()
			continue
		}

		runner.wg.Add(1)
		go runner.handleConn(conn)
	}
}

func (runner *tcpRunner) handleConn(clientConn net.Conn) {
	defer runner.wg.Done()

	runner.trackConn(clientConn)
	runner.activeConn.Add(1)
	runner.collector.IncConns(runner.rule.RuleID)
	defer func() {
		runner.untrackConn(clientConn)
		_ = clientConn.Close()
		runner.activeConn.Add(-1)
		runner.collector.DecConns(runner.rule.RuleID)
	}()

	targetAddr := net.JoinHostPort(strings.TrimSpace(runner.rule.TargetAddr), strconv.Itoa(runner.rule.TargetPort))
	dialer := net.Dialer{Timeout: 10 * time.Second}
	targetConn, err := dialer.DialContext(runner.ctx, "tcp", targetAddr)
	if err != nil {
		return
	}
	runner.trackConn(targetConn)
	defer func() {
		runner.untrackConn(targetConn)
		_ = targetConn.Close()
	}()

	idle := runner.effectiveIdleTimeout()
	var copyWG sync.WaitGroup
	copyWG.Add(2)

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(targetConn, clientConn, runner.rule.SpeedLimit, idle, func(n int64) {
			runner.collector.AddUpload(runner.rule.RuleID, n)
		})
	}()

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(clientConn, targetConn, runner.rule.SpeedLimit, idle, func(n int64) {
			runner.collector.AddDownload(runner.rule.RuleID, n)
		})
	}()

	copyWG.Wait()
}

func (runner *tcpRunner) trackConn(c net.Conn) {
	runner.connMu.Lock()
	runner.conns[c] = struct{}{}
	runner.connMu.Unlock()
}

func (runner *tcpRunner) untrackConn(c net.Conn) {
	runner.connMu.Lock()
	delete(runner.conns, c)
	runner.connMu.Unlock()
}

func (runner *tcpRunner) closeAllConns() {
	runner.connMu.Lock()
	dup := make([]net.Conn, 0, len(runner.conns))
	for c := range runner.conns {
		dup = append(dup, c)
	}
	runner.connMu.Unlock()
	for _, c := range dup {
		_ = c.Close()
	}
}

func (runner *tcpRunner) effectiveIdleTimeout() time.Duration {
	if runner.rule.IdleTimeout > 0 {
		return time.Duration(runner.rule.IdleTimeout) * time.Second
	}
	return DefaultTCPIdleTimeout
}

type rateLimiter struct {
	limit     int64
	allowance float64
	last      time.Time
}

func newRateLimiter(limit int64) *rateLimiter {
	return &rateLimiter{limit: limit, allowance: float64(limit), last: time.Now()}
}

func (limiter *rateLimiter) wait(n int) {
	if limiter == nil || limiter.limit <= 0 || n <= 0 {
		return
	}
	now := time.Now()
	elapsed := now.Sub(limiter.last).Seconds()
	limiter.last = now
	limiter.allowance += elapsed * float64(limiter.limit)
	if limiter.allowance > float64(limiter.limit) {
		limiter.allowance = float64(limiter.limit)
	}
	need := float64(n)
	if limiter.allowance >= need {
		limiter.allowance -= need
		return
	}
	missing := need - limiter.allowance
	sleepSec := missing / float64(limiter.limit)
	if sleepSec > 0 {
		time.Sleep(time.Duration(sleepSec * float64(time.Second)))
	}
	limiter.allowance = 0
	limiter.last = time.Now()
}

func copyWithLimit(dst io.Writer, src io.Reader, speedLimit int64, idle time.Duration, onBytes func(int64)) (int64, error) {
	buf := make([]byte, 32*1024)
	limiter := newRateLimiter(speedLimit)
	var total int64
	for {
		if idle > 0 {
			setReadDeadline(src, time.Now().Add(idle))
		}
		nr, readErr := src.Read(buf)
		if nr > 0 {
			limiter.wait(nr)
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 {
				total += int64(nw)
				if onBytes != nil {
					onBytes(int64(nw))
				}
			}
			if writeErr != nil {
				return total, writeErr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, fmt.Errorf("copy failed: %w", readErr)
		}
	}
}

// setReadDeadline sets a read deadline on src if it is a connection that
// supports deadlines (net.Conn). Used to bound idle time on forwarded conns so
// silent peers cannot hold a connection (and thus a Stop/reload) open forever.
func setReadDeadline(src io.Reader, t time.Time) {
	if d, ok := src.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(t)
	}
}
