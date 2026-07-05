package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultUDPSessionIdle = 60 * time.Second
	maxUDPDatagramSize    = 64 * 1024
)

type udpMessage struct {
	Type     string `json:"type"`
	Version  int    `json:"version,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Token    string `json:"token,omitempty"`
	TunnelID string `json:"tunnel_id,omitempty"`
	ConnID   string `json:"conn_id,omitempty"`
	Payload  string `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type udpRemoteListener struct {
	server   *Server
	clientID string
	spec     TunnelSpec
	key      string
	conn     *net.UDPConn
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*udpServerSession
}

type udpServerSession struct {
	remoteAddr *net.UDPAddr
	connID     string
	dataConn   net.Conn
	writeMu    sync.Mutex
	lastSeen   time.Time
	done       chan struct{}
	once       sync.Once
}

type udpClientSession struct {
	connID     string
	localConn  *net.UDPConn
	serverConn net.Conn
	writeMu    sync.Mutex
	done       chan struct{}
	once       sync.Once
}

func (server *Server) startUDPRemoteListener(ctx context.Context, clientID string, spec TunnelSpec) error {
	listenAddr := net.JoinHostPort(spec.RemoteListenAddr, strconv.Itoa(spec.RemoteListenPort))
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("resolve udp remote %s for tunnel %s: %w", listenAddr, spec.TunnelID, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp remote %s for tunnel %s: %w", listenAddr, spec.TunnelID, err)
	}
	listenerCtx, cancel := context.WithCancel(ctx)
	remote := &udpRemoteListener{server: server, clientID: clientID, spec: spec, key: remoteListenKey(spec), conn: conn, ctx: listenerCtx, cancel: cancel, sessions: make(map[string]*udpServerSession)}
	server.mu.Lock()
	server.udpListeners[remote.key] = remote
	server.mu.Unlock()
	server.collector.EnsureRule(spec.TunnelID)
	remote.wg.Add(2)
	go remote.readLoop()
	go remote.cleanupLoop()
	server.logger.Info("tunnel udp remote listener started", "component", "tunnel_server", "event", "udp_remote_listen", "client_id", clientID, "tunnel_id", spec.TunnelID, "addr", listenAddr)
	return nil
}

func (server *Server) stopUDPRemoteListener(key string) {
	server.mu.Lock()
	listener := server.udpListeners[key]
	if listener != nil {
		delete(server.udpListeners, key)
	}
	server.mu.Unlock()
	if listener == nil {
		return
	}
	listener.cancel()
	_ = listener.conn.Close()
	listener.closeAllSessions()
	listener.wg.Wait()
	server.collector.SetConns(listener.spec.TunnelID, 0)
	server.logger.Info("tunnel udp remote listener stopped", "component", "tunnel_server", "event", "udp_remote_stop", "client_id", listener.clientID, "tunnel_id", listener.spec.TunnelID)
}

func (listener *udpRemoteListener) readLoop() {
	defer listener.wg.Done()
	buf := make([]byte, maxUDPDatagramSize)
	for {
		n, remoteAddr, err := listener.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-listener.ctx.Done():
				return
			default:
				continue
			}
		}
		payload := append([]byte(nil), buf[:n]...)
		listener.wg.Add(1)
		go func() {
			defer listener.wg.Done()
			listener.handleDatagram(remoteAddr, payload)
		}()
	}
}

func (listener *udpRemoteListener) handleDatagram(remoteAddr *net.UDPAddr, payload []byte) {
	session, err := listener.getOrOpenSession(remoteAddr)
	if err != nil {
		listener.server.logger.Warn("tunnel udp session open failed", "component", "tunnel_server", "event", "udp_open_failed", "client_id", listener.clientID, "tunnel_id", listener.spec.TunnelID, "remote_addr", remoteAddr.String(), "error", err)
		return
	}
	session.writeMu.Lock()
	err = writeUDPMessage(session.dataConn, udpMessage{Type: MessageUDP, Version: ProtocolVersion, TunnelID: listener.spec.TunnelID, ConnID: session.connID, Payload: base64.StdEncoding.EncodeToString(payload)})
	session.writeMu.Unlock()
	if err != nil {
		listener.closeSession(remoteAddr.String())
		return
	}
	listener.server.collector.AddUpload(listener.spec.TunnelID, int64(len(payload)))
}

func (listener *udpRemoteListener) getOrOpenSession(remoteAddr *net.UDPAddr) (*udpServerSession, error) {
	key := remoteAddr.String()
	listener.mu.Lock()
	if session := listener.sessions[key]; session != nil {
		session.lastSeen = time.Now()
		listener.mu.Unlock()
		return session, nil
	}
	if listener.spec.MaxConn > 0 && len(listener.sessions) >= listener.spec.MaxConn {
		listener.mu.Unlock()
		return nil, fmt.Errorf("max_conn reached")
	}
	listener.mu.Unlock()

	client := listener.server.getSession(listener.clientID)
	if client == nil {
		return nil, fmt.Errorf("client is not connected")
	}
	connID := newID("udp")
	pendingKey := pendingKey(listener.clientID, listener.spec.TunnelID, connID)
	ch := make(chan net.Conn, 1)
	listener.server.addPending(pendingKey, ch)
	defer listener.server.removePending(pendingKey)
	if err := client.write(Message{Type: MessageOpen, ClientID: listener.clientID, TunnelID: listener.spec.TunnelID, ConnID: connID}); err != nil {
		return nil, err
	}

	var dataConn net.Conn
	select {
	case dataConn = <-ch:
	case <-time.After(listener.server.openTimeout()):
		return nil, fmt.Errorf("open timeout")
	case <-listener.ctx.Done():
		return nil, fmt.Errorf("listener stopped")
	}
	if dataConn == nil {
		return nil, fmt.Errorf("open cancelled")
	}
	session := &udpServerSession{remoteAddr: remoteAddr, connID: connID, dataConn: dataConn, lastSeen: time.Now(), done: make(chan struct{})}

	listener.mu.Lock()
	if existing := listener.sessions[key]; existing != nil {
		listener.mu.Unlock()
		_ = dataConn.Close()
		return existing, nil
	}
	listener.sessions[key] = session
	listener.mu.Unlock()
	listener.server.collector.IncConns(listener.spec.TunnelID)
	listener.wg.Add(1)
	go func() {
		defer listener.wg.Done()
		listener.udpSessionReadLoop(key, session)
	}()
	return session, nil
}

func (listener *udpRemoteListener) udpSessionReadLoop(key string, session *udpServerSession) {
	defer listener.closeSession(key)
	reader := bufio.NewReader(session.dataConn)
	for {
		msg, err := readUDPMessage(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				listener.server.logger.Debug("tunnel udp data read failed", "component", "tunnel_server", "event", "udp_read_failed", "tunnel_id", listener.spec.TunnelID, "conn_id", session.connID, "error", err)
			}
			return
		}
		if msg.Type != MessageUDP {
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(msg.Payload)
		if err != nil {
			continue
		}
		_, _ = listener.conn.WriteToUDP(payload, session.remoteAddr)
		listener.server.collector.AddDownload(listener.spec.TunnelID, int64(len(payload)))
		session.lastSeen = time.Now()
	}
}

func (listener *udpRemoteListener) cleanupLoop() {
	defer listener.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-listener.ctx.Done():
			return
		case <-ticker.C:
			listener.closeIdleSessions(defaultUDPSessionIdle)
		}
	}
}

func (listener *udpRemoteListener) closeIdleSessions(idle time.Duration) {
	now := time.Now()
	keys := make([]string, 0)
	listener.mu.Lock()
	for key, session := range listener.sessions {
		if session != nil && now.Sub(session.lastSeen) > idle {
			keys = append(keys, key)
		}
	}
	listener.mu.Unlock()
	for _, key := range keys {
		listener.closeSession(key)
	}
}

func (listener *udpRemoteListener) closeSession(key string) {
	listener.mu.Lock()
	session := listener.sessions[key]
	if session != nil {
		delete(listener.sessions, key)
	}
	listener.mu.Unlock()
	if session == nil {
		return
	}
	session.once.Do(func() {
		close(session.done)
		_ = session.dataConn.Close()
		listener.server.collector.DecConns(listener.spec.TunnelID)
	})
}

func (listener *udpRemoteListener) closeAllSessions() {
	listener.mu.Lock()
	keys := make([]string, 0, len(listener.sessions))
	for key := range listener.sessions {
		keys = append(keys, key)
	}
	listener.mu.Unlock()
	for _, key := range keys {
		listener.closeSession(key)
	}
}

func runUDPClientSession(ctx context.Context, client *Client, spec TunnelSpec, msg Message, serverConn net.Conn, logger *slog.Logger) {
	localAddr := net.JoinHostPort(spec.LocalAddr, strconv.Itoa(spec.LocalPort))
	udpAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		_ = serverConn.Close()
		return
	}
	localConn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		_ = serverConn.Close()
		return
	}
	session := &udpClientSession{connID: msg.ConnID, localConn: localConn, serverConn: serverConn, done: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		session.readServerLoop(spec, logger)
	}()
	go func() {
		defer wg.Done()
		session.readLocalLoop(spec, logger)
	}()
	go func() {
		select {
		case <-ctx.Done():
			session.close()
		case <-session.done:
		}
	}()
	wg.Wait()
	session.close()
}

func (session *udpClientSession) readServerLoop(spec TunnelSpec, logger *slog.Logger) {
	reader := bufio.NewReader(session.serverConn)
	for {
		msg, err := readUDPMessage(reader)
		if err != nil {
			return
		}
		if msg.Type != MessageUDP {
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(msg.Payload)
		if err != nil {
			continue
		}
		_, _ = session.localConn.Write(payload)
	}
}

func (session *udpClientSession) readLocalLoop(spec TunnelSpec, logger *slog.Logger) {
	buf := make([]byte, maxUDPDatagramSize)
	for {
		n, err := session.localConn.Read(buf)
		if err != nil {
			return
		}
		payload := base64.StdEncoding.EncodeToString(buf[:n])
		session.writeMu.Lock()
		err = writeUDPMessage(session.serverConn, udpMessage{Type: MessageUDP, Version: ProtocolVersion, TunnelID: spec.TunnelID, ConnID: session.connID, Payload: payload})
		session.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

func (session *udpClientSession) close() {
	if session == nil {
		return
	}
	session.once.Do(func() {
		close(session.done)
		if session.localConn != nil {
			_ = session.localConn.Close()
		}
		if session.serverConn != nil {
			_ = session.serverConn.Close()
		}
	})
}

func writeUDPMessage(w io.Writer, msg udpMessage) error {
	msg.Type = strings.ToLower(strings.TrimSpace(msg.Type))
	if msg.Version == 0 {
		msg.Version = ProtocolVersion
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}

func readUDPMessage(reader *bufio.Reader) (udpMessage, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return udpMessage{}, err
	}
	var msg udpMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return udpMessage{}, err
	}
	msg.Type = strings.ToLower(strings.TrimSpace(msg.Type))
	if msg.Version == 0 {
		msg.Version = ProtocolVersion
	}
	if msg.Version != ProtocolVersion {
		return udpMessage{}, fmt.Errorf("unsupported udp protocol version: %d", msg.Version)
	}
	return msg, nil
}
