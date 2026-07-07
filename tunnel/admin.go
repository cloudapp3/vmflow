package tunnel

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

type ClientSnapshot struct {
	ClientID      string `json:"client_id"`
	SessionID     string `json:"session_id"`
	RemoteAddr    string `json:"remote_addr,omitempty"`
	ConnectedTime int64  `json:"connected_time"`
	TunnelCount   int    `json:"tunnel_count"`
}

type TunnelSnapshot struct {
	ClientID         string `json:"client_id"`
	TunnelID         string `json:"tunnel_id"`
	Protocol         string `json:"protocol"`
	RemoteListenAddr string `json:"remote_listen_addr"`
	RemoteListenPort int    `json:"remote_listen_port"`
	LocalAddr        string `json:"local_addr"`
	LocalPort        int    `json:"local_port"`
	ActiveConns      int64  `json:"active_conns"`
	MaxConn          int    `json:"max_conn,omitempty"`
	Remark           string `json:"remark,omitempty"`
}

type ControlHandlerOptions struct {
	ConfigPath string
	Auth       *controlapi.Authenticator
	Logger     *slog.Logger
}

type controlHandler struct {
	server     *Server
	configPath string
	auth       *controlapi.Authenticator
	logger     *slog.Logger
	metrics    *tunnelMetrics
}

func NewControlHandler(server *Server, opts ControlHandlerOptions) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &controlHandler{
		server:     server,
		configPath: strings.TrimSpace(opts.ConfigPath),
		auth:       opts.Auth,
		logger:     logger,
		metrics:    newTunnelMetrics(server),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.HandleFunc("/v1/tunnel/clients", h.handleClients)
	mux.HandleFunc("/v1/tunnel/tunnels", h.handleTunnels)
	mux.HandleFunc("/v1/tunnel/stats", h.handleStats)
	mux.HandleFunc("/v1/tunnel/precheck", h.handlePrecheck)
	mux.HandleFunc("/v1/tunnel/reload", h.handleReload)
	mux.HandleFunc("/metrics", h.handleMetrics)
	return h.withMiddleware(mux)
}

func (h *controlHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeControlJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"running_clients": h.server.RunningClients(),
		"running_tunnels": h.server.RunningTunnels(),
		"time":            time.Now().Unix(),
	})
}

func (h *controlHandler) handleClients(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeControlJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"items": h.server.Clients()})
}

func (h *controlHandler) handleTunnels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeControlJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"items": h.server.Tunnels()})
}

func (h *controlHandler) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeControlJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"items": h.server.Stats()})
}

func (h *controlHandler) handlePrecheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeControlJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	cfg, check, err := h.loadAndPrecheck()
	if err != nil {
		writeControlJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	status := http.StatusOK
	if !check.OK {
		status = http.StatusBadRequest
	}
	writeControlJSON(w, status, map[string]any{
		"config_path":  h.configPath,
		"client_count": len(cfg.TunnelServer.Clients),
		"result":       check,
	})
}

func (h *controlHandler) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeControlJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !h.authorizeWrite(w, r) {
		return
	}
	cfg, _, err := h.loadAndPrecheck()
	if err != nil {
		writeControlJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	result, err := h.server.ReloadConfig(cfg)
	if err != nil {
		writeControlJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	status := http.StatusOK
	if !result.OK {
		status = http.StatusBadRequest
	}
	h.logger.Info("tunnel config reloaded", "component", "tunnel_control", "event", "reload", "client_count", result.ClientCount, "disconnected_clients", len(result.DisconnectedClients))
	writeControlJSON(w, status, map[string]any{
		"config_path": h.configPath,
		"result":      result,
	})
}

func (h *controlHandler) loadAndPrecheck() (ServerConfig, ConfigCheckResult, error) {
	if strings.TrimSpace(h.configPath) == "" {
		return ServerConfig{}, ConfigCheckResult{}, errMissingConfigPath()
	}
	cfg, err := LoadServerConfig(h.configPath)
	if err != nil {
		return ServerConfig{}, ConfigCheckResult{}, err
	}
	return cfg, h.server.PrecheckConfig(cfg), nil
}

func (h *controlHandler) authorizeWrite(w http.ResponseWriter, r *http.Request) bool {
	info, _ := r.Context().Value(tunnelAuthInfoKey{}).(controlapi.AuthInfo)
	if info.Role != controlapi.RoleAdmin {
		writeControlJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return false
	}
	return true
}

func (h *controlHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = h.metrics.Write(w)
}

func (h *controlHandler) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		statusWriter := &controlStatusWriter{ResponseWriter: w, status: http.StatusOK}
		info, ok := h.authenticator().Authenticate(r)
		if !ok {
			statusWriter.status = http.StatusUnauthorized
			writeControlJSON(statusWriter, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			h.logger.Warn("tunnel control authentication failed", "component", "tunnel_control", "event", "auth_failed", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			h.metrics.ObserveControlRequest(r.Method, r.URL.Path, statusWriter.status, time.Since(started))
			return
		}
		next.ServeHTTP(statusWriter, r.WithContext(context.WithValue(r.Context(), tunnelAuthInfoKey{}, info)))
		h.metrics.ObserveControlRequest(r.Method, r.URL.Path, statusWriter.status, time.Since(started))
		h.logger.Debug("tunnel control request", "component", "tunnel_control", "event", "request", "method", r.Method, "path", r.URL.Path, "status", statusWriter.status, "auth_name", info.Name, "auth_role", info.Role)
	})
}

func (h *controlHandler) authenticator() *controlapi.Authenticator {
	if h != nil && h.auth != nil {
		return h.auth
	}
	if h != nil && h.server != nil {
		return controlapi.NewAuthenticator(h.server.Config().Auth)
	}
	return controlapi.NewAuthenticator(config.AuthConfig{})
}

type tunnelAuthInfoKey struct{}

type controlStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *controlStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func writeControlJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func (server *Server) RunningClients() int {
	if server == nil {
		return 0
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.clients)
}

func (server *Server) RunningTunnels() int {
	if server == nil {
		return 0
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.listeners) + len(server.udpListeners)
}

func (server *Server) Clients() []ClientSnapshot {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	tunnelCount := make(map[string]int)
	for _, listener := range server.listeners {
		if listener != nil {
			tunnelCount[listener.clientID]++
		}
	}
	for _, listener := range server.udpListeners {
		if listener != nil {
			tunnelCount[listener.clientID]++
		}
	}
	items := make([]ClientSnapshot, 0, len(server.clients))
	for clientID, session := range server.clients {
		item := ClientSnapshot{ClientID: clientID, TunnelCount: tunnelCount[clientID]}
		if session != nil {
			item.SessionID = session.sessionID
			item.ConnectedTime = session.connectedAt
			if session.conn != nil && session.conn.RemoteAddr() != nil {
				item.RemoteAddr = session.conn.RemoteAddr().String()
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ClientID < items[j].ClientID })
	return items
}

func (server *Server) Tunnels() []TunnelSnapshot {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	items := make([]TunnelSnapshot, 0, len(server.listeners)+len(server.udpListeners))
	for _, listener := range server.listeners {
		if listener == nil {
			continue
		}
		spec := listener.spec
		items = append(items, TunnelSnapshot{
			ClientID:         listener.clientID,
			TunnelID:         spec.TunnelID,
			Protocol:         spec.Protocol,
			RemoteListenAddr: spec.RemoteListenAddr,
			RemoteListenPort: spec.RemoteListenPort,
			LocalAddr:        spec.LocalAddr,
			LocalPort:        spec.LocalPort,
			ActiveConns:      listener.active.Load(),
			MaxConn:          spec.MaxConn,
			Remark:           spec.Remark,
		})
	}
	for _, listener := range server.udpListeners {
		if listener == nil {
			continue
		}
		spec := listener.spec
		listener.mu.Lock()
		activeConns := int64(len(listener.sessions))
		listener.mu.Unlock()
		items = append(items, TunnelSnapshot{
			ClientID:         listener.clientID,
			TunnelID:         spec.TunnelID,
			Protocol:         spec.Protocol,
			RemoteListenAddr: spec.RemoteListenAddr,
			RemoteListenPort: spec.RemoteListenPort,
			LocalAddr:        spec.LocalAddr,
			LocalPort:        spec.LocalPort,
			ActiveConns:      activeConns,
			MaxConn:          spec.MaxConn,
			Remark:           spec.Remark,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ClientID != items[j].ClientID {
			return items[i].ClientID < items[j].ClientID
		}
		return items[i].TunnelID < items[j].TunnelID
	})
	return items
}

func (server *Server) Stats() []engine.TrafficSnapshot {
	if server == nil {
		return nil
	}
	return trafficSnapshots(server.collector)
}

type tunnelMetrics struct {
	server  *Server
	started time.Time
	mu      sync.RWMutex
	control map[tunnelControlRequestKey]*tunnelControlRequestStats
}

type tunnelControlRequestKey struct {
	Method string
	Path   string
	Status string
}

type tunnelControlRequestStats struct {
	Count       int64
	DurationSum float64
}

func newTunnelMetrics(server *Server) *tunnelMetrics {
	return &tunnelMetrics{server: server, started: time.Now(), control: make(map[tunnelControlRequestKey]*tunnelControlRequestStats)}
}

func (m *tunnelMetrics) ObserveControlRequest(method, path string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	key := tunnelControlRequestKey{Method: strings.ToUpper(strings.TrimSpace(method)), Path: normalizeMetricPath(path), Status: strconv.Itoa(status)}
	m.mu.Lock()
	stats := m.control[key]
	if stats == nil {
		stats = &tunnelControlRequestStats{}
		m.control[key] = stats
	}
	stats.Count++
	stats.DurationSum += duration.Seconds()
	m.mu.Unlock()
}

func (m *tunnelMetrics) Write(w http.ResponseWriter) error {
	if m == nil || m.server == nil {
		return nil
	}
	clients := m.server.Clients()
	tunnels := m.server.Tunnels()
	stats := m.server.Stats()
	control := m.copyControl()

	if _, err := w.Write([]byte("# HELP vmflow_tunnel_build_info Static build info for vmflow tunnel.\n# TYPE vmflow_tunnel_build_info gauge\nvmflow_tunnel_build_info 1\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("# HELP vmflow_tunnel_uptime_seconds Time since this vmflow tunnel metrics collector started.\n# TYPE vmflow_tunnel_uptime_seconds gauge\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("vmflow_tunnel_uptime_seconds " + strconv.FormatFloat(time.Since(m.started).Seconds(), 'f', 0, 64) + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("# HELP vmflow_tunnel_clients Current connected tunnel clients.\n# TYPE vmflow_tunnel_clients gauge\nvmflow_tunnel_clients " + strconv.Itoa(len(clients)) + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("# HELP vmflow_tunnel_tunnels Current active tunnel listeners.\n# TYPE vmflow_tunnel_tunnels gauge\nvmflow_tunnel_tunnels " + strconv.Itoa(len(tunnels)) + "\n")); err != nil {
		return err
	}

	if _, err := w.Write([]byte("# HELP vmflow_tunnel_connections Current active connections by tunnel.\n# TYPE vmflow_tunnel_connections gauge\n")); err != nil {
		return err
	}
	for _, sample := range stats {
		t := tunnelForStats(tunnels, sample.RuleID)
		line := "vmflow_tunnel_connections" + tunnelLabels(t) + " " + strconv.FormatInt(sample.Conns, 10) + "\n"
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}

	if _, err := w.Write([]byte("# HELP vmflow_tunnel_upload_bytes_total Total uploaded bytes by tunnel.\n# TYPE vmflow_tunnel_upload_bytes_total counter\n")); err != nil {
		return err
	}
	for _, sample := range stats {
		t := tunnelForStats(tunnels, sample.RuleID)
		line := "vmflow_tunnel_upload_bytes_total" + tunnelLabels(t) + " " + strconv.FormatInt(sample.UploadBytes, 10) + "\n"
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}

	if _, err := w.Write([]byte("# HELP vmflow_tunnel_download_bytes_total Total downloaded bytes by tunnel.\n# TYPE vmflow_tunnel_download_bytes_total counter\n")); err != nil {
		return err
	}
	for _, sample := range stats {
		t := tunnelForStats(tunnels, sample.RuleID)
		line := "vmflow_tunnel_download_bytes_total" + tunnelLabels(t) + " " + strconv.FormatInt(sample.DownloadBytes, 10) + "\n"
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}

	if _, err := w.Write([]byte("# HELP vmflow_tunnel_control_requests_total Total tunnel Control API requests.\n# TYPE vmflow_tunnel_control_requests_total counter\n")); err != nil {
		return err
	}
	for _, key := range sortedTunnelControlKeys(control) {
		line := "vmflow_tunnel_control_requests_total{method=" + strconv.Quote(key.Method) + ",path=" + strconv.Quote(key.Path) + ",status=" + strconv.Quote(key.Status) + "} " + strconv.FormatInt(control[key].Count, 10) + "\n"
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}

	if _, err := w.Write([]byte("# HELP vmflow_tunnel_control_request_duration_seconds_sum Total tunnel Control API request duration in seconds.\n# TYPE vmflow_tunnel_control_request_duration_seconds_sum counter\n")); err != nil {
		return err
	}
	for _, key := range sortedTunnelControlKeys(control) {
		line := "vmflow_tunnel_control_request_duration_seconds_sum{method=" + strconv.Quote(key.Method) + ",path=" + strconv.Quote(key.Path) + ",status=" + strconv.Quote(key.Status) + "} " + strconv.FormatFloat(control[key].DurationSum, 'f', 6, 64) + "\n"
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func (m *tunnelMetrics) copyControl() map[tunnelControlRequestKey]tunnelControlRequestStats {
	out := make(map[tunnelControlRequestKey]tunnelControlRequestStats)
	if m == nil {
		return out
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for key, stats := range m.control {
		if stats != nil {
			out[key] = *stats
		}
	}
	return out
}

func tunnelForStats(tunnels []TunnelSnapshot, ruleID string) TunnelSnapshot {
	for _, item := range tunnels {
		if item.TunnelID == ruleID {
			return item
		}
	}
	return TunnelSnapshot{TunnelID: ruleID, Protocol: "unknown"}
}

func tunnelLabels(t TunnelSnapshot) string {
	return "{client_id=" + strconv.Quote(t.ClientID) + ",tunnel_id=" + strconv.Quote(t.TunnelID) + ",protocol=" + strconv.Quote(t.Protocol) + "}"
}

func sortedTunnelControlKeys(values map[tunnelControlRequestKey]tunnelControlRequestStats) []tunnelControlRequestKey {
	keys := make([]tunnelControlRequestKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Path != keys[j].Path {
			return keys[i].Path < keys[j].Path
		}
		if keys[i].Method != keys[j].Method {
			return keys[i].Method < keys[j].Method
		}
		return keys[i].Status < keys[j].Status
	})
	return keys
}

func normalizeMetricPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	return path
}

func trafficSnapshots(collector *engine.Collector) []engine.TrafficSnapshot {
	if collector == nil {
		return nil
	}
	return collector.SnapshotAll()
}
