package engine

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type httpProxyRunner struct {
	rule      Rule
	collector *Collector

	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener

	activeConn atomic.Int64
	wg         sync.WaitGroup
}

func newHTTPProxyRunner(rule Rule, collector *Collector) Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &httpProxyRunner{
		rule:      rule,
		collector: collector,
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (runner *httpProxyRunner) Start() error {
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

func (runner *httpProxyRunner) Stop() {
	runner.cancel()
	if runner.ln != nil {
		_ = runner.ln.Close()
	}
	runner.wg.Wait()
	runner.collector.SetConns(runner.rule.RuleID, 0)
}

func (runner *httpProxyRunner) acceptLoop() {
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

func (runner *httpProxyRunner) handleConn(clientConn net.Conn) {
	defer runner.wg.Done()

	runner.activeConn.Add(1)
	runner.collector.IncConns(runner.rule.RuleID)
	defer func() {
		_ = clientConn.Close()
		runner.activeConn.Add(-1)
		runner.collector.DecConns(runner.rule.RuleID)
	}()

	clientConn.SetDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReader(clientConn)

	// Peek first line to determine request type
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimSpace(line)

	if strings.HasPrefix(line, "CONNECT ") {
		runner.handleConnect(clientConn, br, line)
	} else {
		runner.handleHTTP(clientConn, br, line)
	}
}

// handleConnect processes HTTPS CONNECT tunnel requests.
func (runner *httpProxyRunner) handleConnect(clientConn net.Conn, br *bufio.Reader, requestLine string) {
	// Parse: CONNECT host:port HTTP/1.x
	parts := strings.SplitN(requestLine, " ", 3)
	if len(parts) < 2 {
		runner.writeError(clientConn, 400, "Bad Request")
		return
	}

	targetAddr := parts[1]
	// Ensure host:port format
	if !strings.Contains(targetAddr, ":") {
		targetAddr = targetAddr + ":443"
	}

	// Drain remaining headers
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(header) == "" {
			break
		}
	}

	// Connect to target
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		runner.writeError(clientConn, 502, "Bad Gateway")
		return
	}
	defer func() { _ = targetConn.Close() }()

	// Send 200 Connection Established
	clientConn.SetDeadline(time.Time{})
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy
	var copyWG sync.WaitGroup
	copyWG.Add(2)

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(targetConn, br, runner.rule.SpeedLimit, func(n int64) {
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

// handleHTTP processes plain HTTP proxy requests.
func (runner *httpProxyRunner) handleHTTP(clientConn net.Conn, br *bufio.Reader, requestLine string) {
	// Parse: METHOD http://host:port/path HTTP/1.x
	parts := strings.SplitN(requestLine, " ", 3)
	if len(parts) < 2 {
		runner.writeError(clientConn, 400, "Bad Request")
		return
	}

	method := parts[0]
	rawURL := parts[1]

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		runner.writeError(clientConn, 400, "Bad Request URL")
		return
	}

	host := parsedURL.Host
	if !strings.Contains(host, ":") {
		if parsedURL.Scheme == "https" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	// Collect remaining headers
	var headers []string
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(header) == "" {
			headers = append(headers, "\r\n")
			break
		}
		headers = append(headers, header)
	}

	// Connect to target
	targetConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		runner.writeError(clientConn, 502, "Bad Gateway")
		return
	}
	defer func() { _ = targetConn.Close() }()

	// Rewrite request line: GET http://host/path → GET /path
	path := parsedURL.Path
	if path == "" {
		path = "/"
	}
	if parsedURL.RawQuery != "" {
		path += "?" + parsedURL.RawQuery
	}
	rewriteLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", method, path)

	// Write rewritten request to target
	clientConn.SetDeadline(time.Time{})
	_, _ = targetConn.Write([]byte(rewriteLine))
	for _, h := range headers {
		_, _ = targetConn.Write([]byte(h))
	}

	// Bidirectional copy
	var copyWG sync.WaitGroup
	copyWG.Add(2)

	go func() {
		defer copyWG.Done()
		_, _ = copyWithLimit(targetConn, br, runner.rule.SpeedLimit, func(n int64) {
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

func (runner *httpProxyRunner) writeError(conn net.Conn, code int, msg string) {
	body := fmt.Sprintf("%d %s", code, msg)
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n\r\n%s",
		code, msg, len(body), body)
	_, _ = conn.Write([]byte(resp))
}
