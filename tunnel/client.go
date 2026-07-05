package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
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

type Client struct {
	cfg    ClientConfig
	logger *slog.Logger
}

type clientControlSession struct {
	client  *Client
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
}

func NewClient(cfg ClientConfig, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{cfg: cfg, logger: logger}
}

func (client *Client) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	minDelay := parseDurationOrDefault(client.cfg.TunnelClient.ReconnectMin, time.Second)
	maxDelay := parseDurationOrDefault(client.cfg.TunnelClient.ReconnectMax, 30*time.Second)
	if maxDelay < minDelay {
		maxDelay = minDelay
	}
	delay := minDelay
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		err := client.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			client.logger.Warn("tunnel client disconnected", "component", "tunnel_client", "event", "disconnected", "error", err, "reconnect_in", delay.String())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

func (client *Client) runOnce(ctx context.Context) error {
	conn, err := client.dialServer(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	session := &clientControlSession{client: client, conn: conn, reader: reader}

	hello := Message{
		Type:     MessageHello,
		Version:  ProtocolVersion,
		ClientID: client.cfg.TunnelClient.ClientID,
		Token:    client.cfg.TunnelClient.Token,
		Tunnels:  client.cfg.TunnelClient.Tunnels,
	}
	if err := session.write(hello); err != nil {
		return err
	}
	setConnDeadline(conn, 15*time.Second)
	accept, err := readMessageLine(reader)
	if err != nil {
		return err
	}
	clearConnDeadline(conn)
	if accept.Type == MessageError {
		return fmt.Errorf("server rejected tunnel client: %s", accept.Error)
	}
	if accept.Type != MessageAccept || !accept.OK {
		return fmt.Errorf("unexpected tunnel server response: %s", accept.Type)
	}
	client.logger.Info("tunnel client connected", "component", "tunnel_client", "event", "connected", "server", client.cfg.TunnelClient.ServerAddr, "client_id", client.cfg.TunnelClient.ClientID, "session_id", accept.SessionID, "tunnel_count", len(client.cfg.TunnelClient.Tunnels))

	errCh := make(chan error, 1)
	go func() { errCh <- session.readLoop(ctx) }()
	select {
	case <-ctx.Done():
		_ = conn.Close()
		return nil
	case err := <-errCh:
		return err
	}
}

func (session *clientControlSession) readLoop(ctx context.Context) error {
	for {
		msg, err := readMessageLine(session.reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return err
			}
			return fmt.Errorf("control read failed: %w", err)
		}
		switch msg.Type {
		case MessageOpen:
			go session.handleOpen(ctx, msg)
		case MessagePing:
			_ = session.write(Message{Type: MessagePong, OK: true})
		case MessageError:
			return fmt.Errorf("server error: %s", msg.Error)
		default:
			session.client.logger.Debug("ignored tunnel server message", "component", "tunnel_client", "event", "message_ignored", "type", msg.Type)
		}
	}
}

func (session *clientControlSession) handleOpen(ctx context.Context, msg Message) {
	spec, ok := session.client.findTunnel(msg.TunnelID)
	if !ok {
		_ = session.write(Message{Type: MessageClose, ClientID: session.client.cfg.TunnelClient.ClientID, TunnelID: msg.TunnelID, ConnID: msg.ConnID, Error: "unknown tunnel"})
		return
	}
	serverConn, err := session.client.dialServer(ctx)
	if err != nil {
		_ = session.write(Message{Type: MessageClose, ClientID: session.client.cfg.TunnelClient.ClientID, TunnelID: msg.TunnelID, ConnID: msg.ConnID, Error: err.Error()})
		return
	}
	dataMsg := Message{Type: MessageData, Version: ProtocolVersion, ClientID: session.client.cfg.TunnelClient.ClientID, Token: session.client.cfg.TunnelClient.Token, TunnelID: spec.TunnelID, ConnID: msg.ConnID}
	if err := writeMessage(serverConn, dataMsg); err != nil {
		_ = serverConn.Close()
		_ = session.write(Message{Type: MessageClose, ClientID: session.client.cfg.TunnelClient.ClientID, TunnelID: msg.TunnelID, ConnID: msg.ConnID, Error: err.Error()})
		return
	}
	session.client.logger.Debug("tunnel data connection opened", "component", "tunnel_client", "event", "data_open", "protocol", spec.Protocol, "tunnel_id", spec.TunnelID, "conn_id", msg.ConnID)
	if spec.Protocol == "udp" {
		runUDPClientSession(ctx, session.client, spec, msg, serverConn, session.client.logger)
		return
	}
	localAddr := net.JoinHostPort(spec.LocalAddr, strconv.Itoa(spec.LocalPort))
	dialTimeout := parseDurationOrDefault(session.client.cfg.TunnelClient.DialTimeout, 10*time.Second)
	localConn, err := net.DialTimeout("tcp", localAddr, dialTimeout)
	if err != nil {
		_ = serverConn.Close()
		_ = session.write(Message{Type: MessageClose, ClientID: session.client.cfg.TunnelClient.ClientID, TunnelID: msg.TunnelID, ConnID: msg.ConnID, Error: err.Error()})
		return
	}
	pipePair(localConn, serverConn, localConn, nil, nil)
}

func (session *clientControlSession) write(msg Message) error {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	return writeMessage(session.conn, msg)
}

func (client *Client) dialServer(ctx context.Context) (net.Conn, error) {
	timeout := parseDurationOrDefault(client.cfg.TunnelClient.DialTimeout, 10*time.Second)
	dialer := &net.Dialer{Timeout: timeout}
	if client.cfg.TunnelClient.TLS.Enabled {
		return tls.DialWithDialer(dialer, "tcp", client.cfg.TunnelClient.ServerAddr, &tls.Config{ServerName: client.cfg.TunnelClient.TLS.ServerName, InsecureSkipVerify: client.cfg.TunnelClient.TLS.InsecureSkipVerify, MinVersion: tls.VersionTLS12})
	}
	return dialer.DialContext(ctx, "tcp", client.cfg.TunnelClient.ServerAddr)
}

func (client *Client) findTunnel(tunnelID string) (TunnelSpec, bool) {
	tunnelID = strings.TrimSpace(tunnelID)
	for _, spec := range client.cfg.TunnelClient.Tunnels {
		if spec.TunnelID == tunnelID {
			return spec, true
		}
	}
	return TunnelSpec{}, false
}

func parseDurationOrDefault(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
