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

type tcpRunner struct {
	rule      Rule
	collector *Collector

	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener

	activeConn atomic.Int64
	wg         sync.WaitGroup
}

func newTCPRunner(rule Rule, collector *Collector) Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &tcpRunner{
		rule:      rule,
		collector: collector,
		ctx:       ctx,
		cancel:    cancel,
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

	runner.activeConn.Add(1)
	runner.collector.IncConns(runner.rule.RuleID)
	defer func() {
		_ = clientConn.Close()
		runner.activeConn.Add(-1)
		runner.collector.DecConns(runner.rule.RuleID)
	}()

	targetAddr := net.JoinHostPort(strings.TrimSpace(runner.rule.TargetAddr), strconv.Itoa(runner.rule.TargetPort))
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		return
	}
	defer func() {
		_ = targetConn.Close()
	}()

	var copyWG sync.WaitGroup
	copyWG.Add(2)

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(targetConn, clientConn, runner.rule.SpeedLimit, func(n int64) {
			runner.collector.AddUpload(runner.rule.RuleID, n)
		})
	}()

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(clientConn, targetConn, runner.rule.SpeedLimit, func(n int64) {
			runner.collector.AddDownload(runner.rule.RuleID, n)
		})
	}()

	copyWG.Wait()
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

func copyWithLimit(dst io.Writer, src io.Reader, speedLimit int64, onBytes func(int64)) (int64, error) {
	buf := make([]byte, 32*1024)
	limiter := newRateLimiter(speedLimit)
	var total int64
	for {
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
