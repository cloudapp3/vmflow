package tunnel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudapp3/vmflow/engine"
)

type Server struct {
	cfg       ServerConfig
	cfgMu     sync.RWMutex
	logger    *slog.Logger
	collector *engine.Collector

	mu           sync.Mutex
	clients      map[string]*serverClientSession
	listeners    map[string]*remoteListener
	udpListeners map[string]*udpRemoteListener
	pending      map[string]chan net.Conn

	listenConn net.Listener
	wg         sync.WaitGroup
}

type serverClientSession struct {
	clientID    string
	sessionID   string
	conn        net.Conn
	reader      *bufio.Reader
	writeMu     sync.Mutex
	done        chan struct{}
	once        sync.Once
	connectedAt int64
	token       string
	tunnels     []TunnelSpec
}

type remoteListener struct {
	server   *Server
	clientID string
	spec     TunnelSpec
	key      string
	ln       net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	active   atomic.Int64
	wg       sync.WaitGroup
}

func NewServer(cfg ServerConfig, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:          cfg,
		logger:       logger,
		collector:    engine.NewCollector(),
		clients:      make(map[string]*serverClientSession),
		listeners:    make(map[string]*remoteListener),
		udpListeners: make(map[string]*udpRemoteListener),
		pending:      make(map[string]chan net.Conn),
	}
}

func (server *Server) Collector() *engine.Collector {
	if server == nil {
		return nil
	}
	return server.collector
}

func (server *Server) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := server.Config()
	listenAddr := strings.TrimSpace(cfg.TunnelServer.ListenAddr)
	if listenAddr == "" {
		listenAddr = DefaultServerListenAddr
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	if cfg.TunnelServer.TLS.Enabled {
		cert, err := tls.LoadX509KeyPair(cfg.TunnelServer.TLS.CertFile, cfg.TunnelServer.TLS.KeyFile)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("load tunnel server tls certificate: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	server.listenConn = ln
	server.logger.Info("vmflow tunnel server listening", "component", "tunnel_server", "event", "listen", "addr", listenAddr, "tls", cfg.TunnelServer.TLS.Enabled)

	errCh := make(chan error, 1)
	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		errCh <- server.acceptTunnelLoop(ctx, ln)
	}()

	select {
	case <-ctx.Done():
		_ = ln.Close()
		server.stopAll()
		server.wg.Wait()
		return nil
	case err := <-errCh:
		server.stopAll()
		server.wg.Wait()
		if err == nil || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (server *Server) acceptTunnelLoop(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		server.wg.Add(1)
		go func() {
			defer server.wg.Done()
			server.handleTunnelConn(ctx, conn)
		}()
	}
}

func (server *Server) handleTunnelConn(ctx context.Context, conn net.Conn) {
	reader := bufio.NewReader(conn)
	setConnDeadline(conn, 15*time.Second)
	msg, err := readMessageLine(reader)
	if err != nil {
		_ = conn.Close()
		return
	}
	clearConnDeadline(conn)

	switch msg.Type {
	case MessageHello:
		server.handleHello(ctx, conn, reader, msg)
	case MessageData:
		if !server.handleDataConn(conn, reader, msg) {
			_ = conn.Close()
		}
	default:
		_ = writeMessage(conn, Message{Type: MessageError, Error: "first message must be hello or data"})
		_ = conn.Close()
	}
}

func (server *Server) handleHello(ctx context.Context, conn net.Conn, reader *bufio.Reader, msg Message) {
	acl, ok := server.authenticate(msg.ClientID, msg.Token)
	if !ok {
		_ = writeMessage(conn, Message{Type: MessageError, Error: "authentication failed"})
		_ = conn.Close()
		server.logger.Warn("tunnel client authentication failed", "component", "tunnel_server", "event", "auth_failed", "client_id", msg.ClientID, "remote", conn.RemoteAddr().String())
		return
	}
	if len(msg.Tunnels) == 0 {
		_ = writeMessage(conn, Message{Type: MessageError, Error: "hello must include at least one tunnel"})
		_ = conn.Close()
		return
	}
	if acl.Allow.MaxTunnels > 0 && len(msg.Tunnels) > acl.Allow.MaxTunnels {
		_ = writeMessage(conn, Message{Type: MessageError, Error: "too many tunnels"})
		_ = conn.Close()
		return
	}
	for i := range msg.Tunnels {
		msg.Tunnels[i] = normalizeTunnelSpec(msg.Tunnels[i])
		if err := validateTunnelSpec(msg.Tunnels[i]); err != nil {
			_ = writeMessage(conn, Message{Type: MessageError, Error: fmt.Sprintf("tunnels[%d]: %v", i, err)})
			_ = conn.Close()
			return
		}
		if err := aclAllows(acl.Allow, msg.Tunnels[i]); err != nil {
			_ = writeMessage(conn, Message{Type: MessageError, Error: fmt.Sprintf("tunnels[%d]: %v", i, err)})
			_ = conn.Close()
			return
		}
	}

	session := &serverClientSession{
		clientID:    msg.ClientID,
		sessionID:   newID("session"),
		conn:        conn,
		reader:      reader,
		done:        make(chan struct{}),
		connectedAt: time.Now().Unix(),
		token:       msg.Token,
		tunnels:     append([]TunnelSpec(nil), msg.Tunnels...),
	}
	if err := server.registerSession(ctx, session, msg.Tunnels); err != nil {
		_ = writeMessage(conn, Message{Type: MessageError, Error: err.Error()})
		_ = conn.Close()
		return
	}
	_ = session.write(Message{Type: MessageAccept, OK: true, SessionID: session.sessionID})
	server.logger.Info("tunnel client connected", "component", "tunnel_server", "event", "client_connected", "client_id", session.clientID, "session_id", session.sessionID, "tunnel_count", len(msg.Tunnels))
	server.controlReadLoop(session)
}

func (server *Server) controlReadLoop(session *serverClientSession) {
	defer func() {
		server.unregisterSession(session.clientID, session)
		_ = session.conn.Close()
		session.closeDone()
		server.logger.Info("tunnel client disconnected", "component", "tunnel_server", "event", "client_disconnected", "client_id", session.clientID, "session_id", session.sessionID)
	}()
	for {
		msg, err := readMessageLine(session.reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				server.logger.Warn("tunnel control read failed", "component", "tunnel_server", "event", "control_read_failed", "client_id", session.clientID, "error", err)
			}
			return
		}
		switch msg.Type {
		case MessagePing:
			_ = session.write(Message{Type: MessagePong, OK: true})
		case MessageClose:
			server.cancelPending(msg.ClientID, msg.TunnelID, msg.ConnID)
		default:
			server.logger.Debug("ignored tunnel control message", "component", "tunnel_server", "event", "control_ignored", "client_id", session.clientID, "type", msg.Type)
		}
	}
}

func (server *Server) registerSession(ctx context.Context, session *serverClientSession, specs []TunnelSpec) error {
	server.mu.Lock()
	previous := server.clients[session.clientID]
	server.mu.Unlock()
	if previous != nil {
		server.unregisterSession(session.clientID, previous)
		_ = previous.conn.Close()
		previous.closeDone()
	}

	started := make([]string, 0, len(specs))
	for _, spec := range specs {
		key := remoteListenKey(spec)
		server.mu.Lock()
		_, tcpExists := server.listeners[key]
		_, udpExists := server.udpListeners[key]
		server.mu.Unlock()
		if tcpExists || udpExists {
			for _, startedKey := range started {
				server.stopAnyRemoteListener(startedKey)
			}
			return fmt.Errorf("remote listener already in use: %s", key)
		}
		var err error
		if spec.Protocol == "udp" {
			err = server.startUDPRemoteListener(ctx, session.clientID, spec)
		} else {
			err = server.startRemoteListener(ctx, session.clientID, spec)
		}
		if err != nil {
			for _, startedKey := range started {
				server.stopAnyRemoteListener(startedKey)
			}
			return err
		}
		started = append(started, key)
	}

	server.mu.Lock()
	server.clients[session.clientID] = session
	server.mu.Unlock()
	return nil
}

func (server *Server) unregisterSession(clientID string, session *serverClientSession) {
	server.mu.Lock()
	current := server.clients[clientID]
	if current != session {
		server.mu.Unlock()
		return
	}
	delete(server.clients, clientID)
	keys := make([]string, 0)
	for key, listener := range server.listeners {
		if listener.clientID == clientID {
			keys = append(keys, key)
		}
	}
	for key, listener := range server.udpListeners {
		if listener.clientID == clientID {
			keys = append(keys, key)
		}
	}
	server.mu.Unlock()
	for _, key := range keys {
		server.stopAnyRemoteListener(key)
	}
}

func (server *Server) startRemoteListener(ctx context.Context, clientID string, spec TunnelSpec) error {
	listenAddr := net.JoinHostPort(spec.RemoteListenAddr, strconv.Itoa(spec.RemoteListenPort))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen remote %s for tunnel %s: %w", listenAddr, spec.TunnelID, err)
	}
	listenerCtx, cancel := context.WithCancel(ctx)
	remote := &remoteListener{server: server, clientID: clientID, spec: spec, key: remoteListenKey(spec), ln: ln, ctx: listenerCtx, cancel: cancel}
	server.mu.Lock()
	server.listeners[remote.key] = remote
	server.mu.Unlock()
	server.collector.EnsureRule(spec.TunnelID)
	remote.wg.Add(1)
	go remote.acceptLoop()
	server.logger.Info("tunnel remote listener started", "component", "tunnel_server", "event", "remote_listen", "client_id", clientID, "tunnel_id", spec.TunnelID, "addr", listenAddr)
	return nil
}

func (server *Server) stopAnyRemoteListener(key string) {
	server.stopRemoteListener(key)
	server.stopUDPRemoteListener(key)
}

func (server *Server) stopRemoteListener(key string) {
	server.mu.Lock()
	listener := server.listeners[key]
	if listener != nil {
		delete(server.listeners, key)
	}
	server.mu.Unlock()
	if listener == nil {
		return
	}
	listener.cancel()
	_ = listener.ln.Close()
	listener.wg.Wait()
	server.collector.SetConns(listener.spec.TunnelID, 0)
	server.logger.Info("tunnel remote listener stopped", "component", "tunnel_server", "event", "remote_stop", "client_id", listener.clientID, "tunnel_id", listener.spec.TunnelID)
}

func (listener *remoteListener) acceptLoop() {
	defer listener.wg.Done()
	for {
		conn, err := listener.ln.Accept()
		if err != nil {
			select {
			case <-listener.ctx.Done():
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		if listener.spec.MaxConn > 0 && listener.active.Load() >= int64(listener.spec.MaxConn) {
			_ = conn.Close()
			continue
		}
		listener.wg.Add(1)
		go func() {
			defer listener.wg.Done()
			listener.handlePublicConn(conn)
		}()
	}
}

func (listener *remoteListener) handlePublicConn(publicConn net.Conn) {
	listener.active.Add(1)
	listener.server.collector.IncConns(listener.spec.TunnelID)
	defer func() {
		listener.active.Add(-1)
		listener.server.collector.DecConns(listener.spec.TunnelID)
	}()

	client := listener.server.getSession(listener.clientID)
	if client == nil {
		_ = publicConn.Close()
		return
	}
	connID := newID("conn")
	pendingKey := pendingKey(listener.clientID, listener.spec.TunnelID, connID)
	ch := make(chan net.Conn, 1)
	listener.server.addPending(pendingKey, ch)
	defer listener.server.removePending(pendingKey)

	if err := client.write(Message{Type: MessageOpen, ClientID: listener.clientID, TunnelID: listener.spec.TunnelID, ConnID: connID}); err != nil {
		_ = publicConn.Close()
		return
	}

	openTimeout := listener.server.openTimeout()
	var dataConn net.Conn
	select {
	case dataConn = <-ch:
	case <-time.After(openTimeout):
		_ = publicConn.Close()
		listener.server.logger.Warn("tunnel data open timed out", "component", "tunnel_server", "event", "open_timeout", "client_id", listener.clientID, "tunnel_id", listener.spec.TunnelID, "conn_id", connID)
		return
	case <-listener.ctx.Done():
		_ = publicConn.Close()
		return
	}
	if dataConn == nil {
		_ = publicConn.Close()
		return
	}
	pipePair(publicConn, dataConn, publicConn, func(n int64) {
		listener.server.collector.AddUpload(listener.spec.TunnelID, n)
	}, func(n int64) {
		listener.server.collector.AddDownload(listener.spec.TunnelID, n)
	})
}

func (server *Server) handleDataConn(conn net.Conn, reader *bufio.Reader, msg Message) bool {
	if _, ok := server.authenticate(msg.ClientID, msg.Token); !ok {
		return false
	}
	if msg.TunnelID == "" || msg.ConnID == "" {
		return false
	}
	ch := server.getPending(pendingKey(msg.ClientID, msg.TunnelID, msg.ConnID))
	if ch == nil {
		return false
	}
	dataConn := net.Conn(conn)
	if reader != nil && reader.Buffered() > 0 {
		dataConn = &bufferedConn{Conn: conn, reader: reader}
	}
	select {
	case ch <- dataConn:
		return true
	default:
		return false
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (conn *bufferedConn) Read(p []byte) (int, error) {
	if conn.reader != nil && conn.reader.Buffered() > 0 {
		return conn.reader.Read(p)
	}
	return conn.Conn.Read(p)
}

func (server *Server) authenticate(clientID string, token string) (ServerClientACL, bool) {
	clientID = strings.TrimSpace(clientID)
	token = strings.TrimSpace(token)
	cfg := server.Config()
	for _, acl := range cfg.TunnelServer.Clients {
		if acl.ClientID == clientID && subtle.ConstantTimeCompare([]byte(acl.Token), []byte(token)) == 1 {
			return acl, true
		}
	}
	return ServerClientACL{}, false
}

func aclAllows(allow AllowConfig, spec TunnelSpec) error {
	if len(allow.Protocols) > 0 {
		ok := false
		for _, protocol := range allow.Protocols {
			if strings.EqualFold(protocol, spec.Protocol) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("protocol %s is not allowed", spec.Protocol)
		}
	}
	if len(allow.RemotePorts) > 0 {
		ok := false
		for _, port := range allow.RemotePorts {
			if port == spec.RemoteListenPort {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("remote port %d is not allowed", spec.RemoteListenPort)
		}
	}
	return nil
}

func (server *Server) getSession(clientID string) *serverClientSession {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.clients[clientID]
}

func (server *Server) addPending(key string, ch chan net.Conn) {
	server.mu.Lock()
	server.pending[key] = ch
	server.mu.Unlock()
}

func (server *Server) getPending(key string) chan net.Conn {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.pending[key]
}

func (server *Server) removePending(key string) {
	server.mu.Lock()
	delete(server.pending, key)
	server.mu.Unlock()
}

func (server *Server) cancelPending(clientID, tunnelID, connID string) {
	ch := server.getPending(pendingKey(clientID, tunnelID, connID))
	if ch == nil {
		return
	}
	select {
	case ch <- nil:
	default:
	}
}

func (server *Server) stopAll() {
	if server.listenConn != nil {
		_ = server.listenConn.Close()
	}
	server.mu.Lock()
	clients := make([]*serverClientSession, 0, len(server.clients))
	for _, client := range server.clients {
		clients = append(clients, client)
	}
	listenerKeys := make([]string, 0, len(server.listeners)+len(server.udpListeners))
	for key := range server.listeners {
		listenerKeys = append(listenerKeys, key)
	}
	for key := range server.udpListeners {
		listenerKeys = append(listenerKeys, key)
	}
	pending := make([]chan net.Conn, 0, len(server.pending))
	for _, ch := range server.pending {
		pending = append(pending, ch)
	}
	server.mu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- nil:
		default:
		}
	}
	for _, client := range clients {
		_ = client.conn.Close()
		client.closeDone()
	}
	for _, key := range listenerKeys {
		server.stopAnyRemoteListener(key)
	}
}

func (server *Server) openTimeout() time.Duration {
	cfg := server.Config()
	value := strings.TrimSpace(cfg.TunnelServer.OpenTimeout)
	if value == "" {
		value = DefaultOpenTimeout
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

func (session *serverClientSession) write(msg Message) error {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	return writeMessage(session.conn, msg)
}

func (session *serverClientSession) closeDone() {
	session.once.Do(func() { close(session.done) })
}

func pendingKey(clientID, tunnelID, connID string) string {
	return strings.TrimSpace(clientID) + ":" + strings.TrimSpace(tunnelID) + ":" + strings.TrimSpace(connID)
}
