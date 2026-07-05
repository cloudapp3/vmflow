package tunnel

import (
	"crypto/subtle"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type ConfigCheckItem struct {
	Level   string `json:"level"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type ConfigCheckResult struct {
	OK           bool              `json:"ok"`
	ErrorCount   int               `json:"error_count"`
	WarningCount int               `json:"warning_count"`
	Items        []ConfigCheckItem `json:"items"`
}

type ReloadResult struct {
	OK                  bool              `json:"ok"`
	ClientCount         int               `json:"client_count"`
	DisconnectedClients []string          `json:"disconnected_clients,omitempty"`
	Check               ConfigCheckResult `json:"check"`
}

func (server *Server) PrecheckConfig(next ServerConfig) ConfigCheckResult {
	if server == nil {
		return ConfigCheckResult{OK: false, ErrorCount: 1, Items: []ConfigCheckItem{{Level: "error", Message: "server unavailable"}}}
	}
	next, err := normalizeServerConfig(next)
	if err != nil {
		return ConfigCheckResult{OK: false, ErrorCount: 1, Items: []ConfigCheckItem{{Level: "error", Message: err.Error()}}}
	}
	current := server.Config()
	result := ConfigCheckResult{OK: true}
	if strings.TrimSpace(current.AdminListenAddr) != "" && current.AdminListenAddr != next.AdminListenAddr {
		result.addWarning("admin_listen_addr", "admin listener address changes require restart and are not applied by reload")
	}
	if strings.TrimSpace(current.TunnelServer.ListenAddr) != "" && current.TunnelServer.ListenAddr != next.TunnelServer.ListenAddr {
		result.addWarning("tunnel_server.listen_addr", "tunnel server listen address changes require restart and are not applied by reload")
	}
	if !reflect.DeepEqual(current.TunnelServer.TLS, next.TunnelServer.TLS) {
		result.addWarning("tunnel_server.tls", "tunnel server TLS changes require restart and are not applied by reload")
	}
	for _, client := range server.Clients() {
		if _, ok := clientACL(next, client.ClientID); !ok {
			result.addWarning("tunnel_server.clients", "connected client "+client.ClientID+" is not present in the new config and will be disconnected")
		}
	}
	result.OK = result.ErrorCount == 0
	return result
}

func (server *Server) ReloadConfig(next ServerConfig) (ReloadResult, error) {
	if server == nil {
		return ReloadResult{}, nil
	}
	next, err := normalizeServerConfig(next)
	if err != nil {
		return ReloadResult{}, err
	}
	check := server.PrecheckConfig(next)
	if !check.OK {
		return ReloadResult{OK: false, Check: check}, nil
	}
	current := server.Config()
	runtimeCfg := next
	if strings.TrimSpace(current.AdminListenAddr) != "" {
		runtimeCfg.AdminListenAddr = current.AdminListenAddr
	}
	if strings.TrimSpace(current.TunnelServer.ListenAddr) != "" {
		runtimeCfg.TunnelServer.ListenAddr = current.TunnelServer.ListenAddr
	}
	runtimeCfg.TunnelServer.TLS = current.TunnelServer.TLS

	server.cfgMu.Lock()
	server.cfg = runtimeCfg
	server.cfgMu.Unlock()

	disconnected := server.disconnectInvalidSessions(runtimeCfg)
	return ReloadResult{OK: true, ClientCount: len(runtimeCfg.TunnelServer.Clients), DisconnectedClients: disconnected, Check: check}, nil
}

func (server *Server) Config() ServerConfig {
	if server == nil {
		return ServerConfig{}
	}
	server.cfgMu.RLock()
	defer server.cfgMu.RUnlock()
	return server.cfg
}

func (server *Server) disconnectInvalidSessions(cfg ServerConfig) []string {
	server.mu.Lock()
	sessions := make([]*serverClientSession, 0, len(server.clients))
	for _, session := range server.clients {
		if session != nil {
			sessions = append(sessions, session)
		}
	}
	server.mu.Unlock()

	disconnected := make([]string, 0)
	for _, session := range sessions {
		if server.sessionAllowedByConfig(session, cfg) {
			continue
		}
		server.unregisterSession(session.clientID, session)
		if session.conn != nil {
			_ = session.conn.Close()
		}
		session.closeDone()
		disconnected = append(disconnected, session.clientID)
	}
	sort.Strings(disconnected)
	return disconnected
}

func (server *Server) sessionAllowedByConfig(session *serverClientSession, cfg ServerConfig) bool {
	if session == nil {
		return false
	}
	acl, ok := clientACL(cfg, session.clientID)
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(acl.Token), []byte(session.token)) != 1 {
		return false
	}
	if acl.Allow.MaxTunnels > 0 && len(session.tunnels) > acl.Allow.MaxTunnels {
		return false
	}
	for _, spec := range session.tunnels {
		if err := aclAllows(acl.Allow, spec); err != nil {
			return false
		}
	}
	return true
}

func clientACL(cfg ServerConfig, clientID string) (ServerClientACL, bool) {
	clientID = strings.TrimSpace(clientID)
	for _, item := range cfg.TunnelServer.Clients {
		if item.ClientID == clientID {
			return item, true
		}
	}
	return ServerClientACL{}, false
}

func (result *ConfigCheckResult) addWarning(field string, message string) {
	result.WarningCount++
	result.Items = append(result.Items, ConfigCheckItem{Level: "warning", Field: field, Message: message})
}

func errMissingConfigPath() error {
	return fmt.Errorf("missing tunnel server config path")
}
