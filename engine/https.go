package engine

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type httpsRunner struct {
	rule      Rule
	collector *Collector
	certMgr   CertProvider

	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener

	activeConn atomic.Int64
	wg         sync.WaitGroup
}

func newHTTPSRunner(rule Rule, collector *Collector, certMgr CertProvider) Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &httpsRunner{
		rule:      rule,
		collector: collector,
		certMgr:   certMgr,
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (runner *httpsRunner) Start() error {
	// Obtain certificate before starting
	if runner.certMgr != nil && len(runner.rule.Domains) > 0 {
		if err := runner.certMgr.Obtain(runner.ctx, runner.rule.Domains); err != nil {
			return err
		}
	}

	listenAddr := net.JoinHostPort(strings.TrimSpace(runner.rule.ListenAddr), strconv.Itoa(runner.rule.ListenPort))

	tlsConfig := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if runner.certMgr == nil {
				return nil, fmt.Errorf("no certificate manager")
			}
			return runner.certMgr.GetCertificate(hello)
		},
	}

	rawLn, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	runner.ln = tls.NewListener(rawLn, tlsConfig)

	runner.wg.Add(1)
	go runner.acceptLoop()
	return nil
}

func (runner *httpsRunner) Stop() {
	runner.cancel()
	if runner.ln != nil {
		_ = runner.ln.Close()
	}
	runner.wg.Wait()
	runner.collector.SetConns(runner.rule.RuleID, 0)
}

func (runner *httpsRunner) acceptLoop() {
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

func (runner *httpsRunner) handleConn(clientConn net.Conn) {
	defer runner.wg.Done()

	runner.activeConn.Add(1)
	runner.collector.IncConns(runner.rule.RuleID)
	defer func() {
		_ = clientConn.Close()
		runner.activeConn.Add(-1)
		runner.collector.DecConns(runner.rule.RuleID)
	}()

	// Set deadline for the initial request
	clientConn.SetDeadline(time.Now().Add(30 * time.Second))

	// TLS handshake already done by tls.Listener.
	// Read the HTTP request to forward it.
	br := bufio.NewReader(clientConn)

	// Read and forward the entire request as-is (transparent reverse proxy)
	targetAddr := net.JoinHostPort(strings.TrimSpace(runner.rule.TargetAddr), strconv.Itoa(runner.rule.TargetPort))
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		log.Printf("[https] dial %s failed: %v", targetAddr, err)
		return
	}
	defer func() { _ = targetConn.Close() }()

	clientConn.SetDeadline(time.Time{})

	var copyWG sync.WaitGroup
	copyWG.Add(2)

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(runner.ctx, targetConn, br, runner.rule.SpeedLimit, time.Duration(runner.rule.IdleTimeout)*time.Second, func(n int64) {
			runner.collector.AddUpload(runner.rule.RuleID, n)
		})
	}()

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(runner.ctx, clientConn, targetConn, runner.rule.SpeedLimit, time.Duration(runner.rule.IdleTimeout)*time.Second, func(n int64) {
			runner.collector.AddDownload(runner.rule.RuleID, n)
		})
	}()

	copyWG.Wait()
}
