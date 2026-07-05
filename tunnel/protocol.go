package tunnel

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const ProtocolVersion = 1

const (
	MessageHello  = "hello"
	MessageAccept = "accept"
	MessageOpen   = "open"
	MessageData   = "data"
	MessageUDP    = "udp"
	MessageClose  = "close"
	MessagePing   = "ping"
	MessagePong   = "pong"
	MessageError  = "error"
)

type Message struct {
	Type      string       `json:"type"`
	Version   int          `json:"version,omitempty"`
	ClientID  string       `json:"client_id,omitempty"`
	Token     string       `json:"token,omitempty"`
	SessionID string       `json:"session_id,omitempty"`
	TunnelID  string       `json:"tunnel_id,omitempty"`
	ConnID    string       `json:"conn_id,omitempty"`
	Tunnels   []TunnelSpec `json:"tunnels,omitempty"`
	OK        bool         `json:"ok,omitempty"`
	Error     string       `json:"error,omitempty"`
	Time      int64        `json:"time,omitempty"`
}

func normalizeMessage(msg Message) Message {
	msg.Type = strings.ToLower(strings.TrimSpace(msg.Type))
	msg.ClientID = strings.TrimSpace(msg.ClientID)
	msg.Token = strings.TrimSpace(msg.Token)
	msg.SessionID = strings.TrimSpace(msg.SessionID)
	msg.TunnelID = strings.TrimSpace(msg.TunnelID)
	msg.ConnID = strings.TrimSpace(msg.ConnID)
	if msg.Version == 0 {
		msg.Version = ProtocolVersion
	}
	return msg
}

func writeMessage(w io.Writer, msg Message) error {
	msg = normalizeMessage(msg)
	if msg.Time == 0 {
		msg.Time = time.Now().Unix()
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}

func readMessageLine(reader *bufio.Reader) (Message, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return Message{}, err
	}
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return Message{}, fmt.Errorf("empty message")
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return Message{}, err
	}
	msg = normalizeMessage(msg)
	if msg.Version != ProtocolVersion {
		return Message{}, fmt.Errorf("unsupported tunnel protocol version: %d", msg.Version)
	}
	if msg.Type == "" {
		return Message{}, fmt.Errorf("missing message type")
	}
	return msg, nil
}

func newID(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf[:])
}
