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
	rule  Rule
	stats boundRuleStats

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
		rule:   rule,
		stats:  collector.bindRule(rule.RuleID),
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[net.Conn]struct{}),
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

		if !runner.reserveConnSlot() {
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
	runner.stats.incConns()
	defer func() {
		runner.untrackConn(clientConn)
		_ = clientConn.Close()
		runner.releaseConnSlot()
		runner.stats.decConns()
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

	runner.relayConn(clientConn, targetConn)
}

func (runner *tcpRunner) reserveConnSlot() bool {
	maxConn := int64(runner.rule.MaxConn)
	if maxConn <= 0 {
		runner.activeConn.Add(1)
		return true
	}
	for {
		current := runner.activeConn.Load()
		if current >= maxConn {
			return false
		}
		if runner.activeConn.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (runner *tcpRunner) releaseConnSlot() {
	runner.activeConn.Add(-1)
}

type tcpCopyResult struct {
	dst net.Conn
	err error
}

type tcpActivity struct {
	started     time.Time
	lastElapsed atomic.Int64
	pending     atomic.Int64
}

func newTCPActivity() *tcpActivity {
	activity := &tcpActivity{started: time.Now()}
	activity.touch()
	return activity
}

func (activity *tcpActivity) touch() {
	activity.lastElapsed.Store(time.Since(activity.started).Nanoseconds())
}

func (activity *tcpActivity) idleFor(now time.Time) time.Duration {
	return now.Sub(activity.started) - time.Duration(activity.lastElapsed.Load())
}

func (activity *tcpActivity) setPending(pending bool) {
	if pending {
		activity.pending.Add(1)
		return
	}
	activity.touch()
	activity.pending.Add(-1)
}

func (activity *tcpActivity) hasPending() bool {
	return activity.pending.Load() > 0
}

func (runner *tcpRunner) relayConn(clientConn, targetConn net.Conn) {
	idle := runner.effectiveIdleTimeout()
	activity := newTCPActivity()
	connCtx, cancel := context.WithCancel(runner.ctx)
	defer cancel()

	results := make(chan tcpCopyResult, 2)
	startCopy := func(dst, src net.Conn, onBytes func(int64)) {
		go func() {
			_, err := copyTCPDirection(connCtx, dst, src, runner.rule.SpeedLimit, idle, activity, onBytes)
			results <- tcpCopyResult{dst: dst, err: err}
		}()
	}
	startCopy(targetConn, clientConn, func(n int64) {
		runner.stats.addUpload(n)
	})
	startCopy(clientConn, targetConn, func(n int64) {
		runner.stats.addDownload(n)
	})

	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if idle > 0 {
		idleTimer = time.NewTimer(idle)
		idleC = idleTimer.C
		defer idleTimer.Stop()
	}
	ctxDone := connCtx.Done()
	remaining := 2
	closing := false
	closeBoth := func() {
		if closing {
			return
		}
		closing = true
		cancel()
		_ = clientConn.Close()
		_ = targetConn.Close()
		ctxDone = nil
		idleC = nil
	}

	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if closing {
				continue
			}
			if result.err != nil {
				closeBoth()
				continue
			}
			closeTCPWrite(result.dst)
		case <-ctxDone:
			closeBoth()
		case <-idleC:
			now := time.Now()
			if activity.hasPending() {
				idleTimer.Reset(idle)
				continue
			}
			idleFor := activity.idleFor(now)
			if idleFor >= idle {
				closeBoth()
				continue
			}
			idleTimer.Reset(idle - idleFor)
		}
	}
}

func closeTCPWrite(conn net.Conn) {
	if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
	}
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

const tcpCopyBufferSize = 32 * 1024

type tcpCopyBuffer [tcpCopyBufferSize]byte

var tcpCopyBufferPool = sync.Pool{
	New: func() any { return new(tcpCopyBuffer) },
}

func newRateLimiter(limit int64) *rateLimiter {
	return &rateLimiter{limit: limit, allowance: float64(limit), last: time.Now()}
}

func (limiter *rateLimiter) wait(ctx context.Context, n int) error {
	if limiter == nil || limiter.limit <= 0 || n <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
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
		return nil
	}
	missing := need - limiter.allowance
	sleepSec := missing / float64(limiter.limit)
	if sleepSec > 0 {
		timer := time.NewTimer(time.Duration(sleepSec * float64(time.Second)))
		defer func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	limiter.allowance = 0
	limiter.last = time.Now()
	return nil
}

func copyWithLimit(ctx context.Context, dst io.Writer, src io.Reader, speedLimit int64, idle time.Duration, onBytes func(int64)) (int64, error) {
	return copyWithOptions(ctx, dst, src, speedLimit, copyOptions{readIdle: idle}, onBytes)
}

type copyOptions struct {
	readIdle     time.Duration
	writeTimeout time.Duration
	onActivity   func()
	onPending    func(bool)
}

func copyTCPDirection(ctx context.Context, dst, src net.Conn, speedLimit int64, writeTimeout time.Duration, activity *tcpActivity, onBytes func(int64)) (int64, error) {
	return copyWithOptions(ctx, dst, src, speedLimit, copyOptions{
		writeTimeout: writeTimeout,
		onActivity:   activity.touch,
		onPending:    activity.setPending,
	}, onBytes)
}

func copyWithOptions(ctx context.Context, dst io.Writer, src io.Reader, speedLimit int64, options copyOptions, onBytes func(int64)) (int64, error) {
	buf := tcpCopyBufferPool.Get().(*tcpCopyBuffer)
	defer tcpCopyBufferPool.Put(buf)
	limiter := newRateLimiter(speedLimit)
	var writeDeadline time.Time
	var total int64
	for {
		if options.readIdle > 0 {
			setReadDeadline(src, time.Now().Add(options.readIdle))
		}
		nr, readErr := src.Read(buf[:])
		if nr > 0 {
			if options.onActivity != nil {
				options.onActivity()
			}
			if options.onPending != nil && speedLimit > 0 {
				options.onPending(true)
			}
			limitErr := limiter.wait(ctx, nr)
			if options.onPending != nil && speedLimit > 0 {
				options.onPending(false)
			}
			if limitErr != nil {
				return total, limitErr
			}
			if options.writeTimeout > 0 {
				now := time.Now()
				if writeDeadline.Sub(now) <= options.writeTimeout/2 {
					writeDeadline = now.Add(options.writeTimeout)
					setWriteDeadline(dst, writeDeadline)
				}
			}
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 {
				if options.onActivity != nil {
					options.onActivity()
				}
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

func setWriteDeadline(dst io.Writer, t time.Time) {
	if d, ok := dst.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(t)
	}
}
