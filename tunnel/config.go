package tunnel

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"gopkg.in/yaml.v3"
)

const (
	DefaultServerListenAddr        = "0.0.0.0:18080"
	DefaultTunnelControlListenAddr = "127.0.0.1:19091"
	DefaultDialTimeout             = "10s"
	DefaultOpenTimeout             = "10s"
	DefaultReconnectMin            = "1s"
	DefaultReconnectMax            = "30s"
)

type ServerConfig struct {
	Version           int                `json:"version" yaml:"version"`
	ControlListenAddr string             `json:"control_listen_addr,omitempty" yaml:"control_listen_addr,omitempty"`
	Log               config.LogConfig   `json:"log,omitempty" yaml:"log,omitempty"`
	Auth              config.AuthConfig  `json:"auth,omitempty" yaml:"auth,omitempty"`
	TunnelServer      TunnelServerConfig `json:"tunnel_server" yaml:"tunnel_server"`
}

type TunnelServerConfig struct {
	Enabled     bool              `json:"enabled" yaml:"enabled"`
	ListenAddr  string            `json:"listen_addr" yaml:"listen_addr"`
	TLS         TLSConfig         `json:"tls,omitempty" yaml:"tls,omitempty"`
	OpenTimeout string            `json:"open_timeout,omitempty" yaml:"open_timeout,omitempty"`
	Clients     []ServerClientACL `json:"clients" yaml:"clients"`
}

type TLSConfig struct {
	Enabled            bool   `json:"enabled" yaml:"enabled"`
	CertFile           string `json:"cert_file,omitempty" yaml:"cert_file,omitempty"`
	KeyFile            string `json:"key_file,omitempty" yaml:"key_file,omitempty"`
	ServerName         string `json:"server_name,omitempty" yaml:"server_name,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
}

type ServerClientACL struct {
	ClientID string      `json:"client_id" yaml:"client_id"`
	Token    string      `json:"token" yaml:"token"`
	Allow    AllowConfig `json:"allow,omitempty" yaml:"allow,omitempty"`
}

type AllowConfig struct {
	Protocols   []string `json:"protocols,omitempty" yaml:"protocols,omitempty"`
	RemotePorts []int    `json:"remote_ports,omitempty" yaml:"remote_ports,omitempty"`
	MaxTunnels  int      `json:"max_tunnels,omitempty" yaml:"max_tunnels,omitempty"`
}

type ClientConfig struct {
	Version      int                `json:"version" yaml:"version"`
	Log          config.LogConfig   `json:"log,omitempty" yaml:"log,omitempty"`
	TunnelClient TunnelClientConfig `json:"tunnel_client" yaml:"tunnel_client"`
}

type TunnelClientConfig struct {
	Enabled      bool         `json:"enabled" yaml:"enabled"`
	ServerAddr   string       `json:"server_addr" yaml:"server_addr"`
	TLS          TLSConfig    `json:"tls,omitempty" yaml:"tls,omitempty"`
	ClientID     string       `json:"client_id" yaml:"client_id"`
	Token        string       `json:"token" yaml:"token"`
	DialTimeout  string       `json:"dial_timeout,omitempty" yaml:"dial_timeout,omitempty"`
	ReconnectMin string       `json:"reconnect_min,omitempty" yaml:"reconnect_min,omitempty"`
	ReconnectMax string       `json:"reconnect_max,omitempty" yaml:"reconnect_max,omitempty"`
	Tunnels      []TunnelSpec `json:"tunnels" yaml:"tunnels"`
}

type TunnelSpec struct {
	TunnelID         string `json:"tunnel_id" yaml:"tunnel_id"`
	Protocol         string `json:"protocol" yaml:"protocol"`
	RemoteListenAddr string `json:"remote_listen_addr" yaml:"remote_listen_addr"`
	RemoteListenPort int    `json:"remote_listen_port" yaml:"remote_listen_port"`
	LocalAddr        string `json:"local_addr" yaml:"local_addr"`
	LocalPort        int    `json:"local_port" yaml:"local_port"`
	MaxConn          int    `json:"max_conn,omitempty" yaml:"max_conn,omitempty"`
	SpeedLimit       int64  `json:"speed_limit,omitempty" yaml:"speed_limit,omitempty"`
	Remark           string `json:"remark,omitempty" yaml:"remark,omitempty"`
}

func LoadServerConfig(path string) (ServerConfig, error) {
	raw, err := readConfig(path)
	if err != nil {
		return ServerConfig{}, err
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return ServerConfig{}, err
	}
	return normalizeServerConfig(cfg)
}

func LoadClientConfig(path string) (ClientConfig, error) {
	raw, err := readConfig(path)
	if err != nil {
		return ClientConfig{}, err
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return ClientConfig{}, err
	}
	return normalizeClientConfig(cfg)
}

func readConfig(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("missing config path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty config")
	}
	return raw, nil
}

func normalizeServerConfig(cfg ServerConfig) (ServerConfig, error) {
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Version != 1 {
		return ServerConfig{}, fmt.Errorf("unsupported config version: %d", cfg.Version)
	}
	cfg.ControlListenAddr = strings.TrimSpace(cfg.ControlListenAddr)
	if cfg.ControlListenAddr == "" {
		cfg.ControlListenAddr = DefaultTunnelControlListenAddr
	}
	if _, _, err := net.SplitHostPort(cfg.ControlListenAddr); err != nil {
		return ServerConfig{}, fmt.Errorf("control_listen_addr: %w", err)
	}
	var err error
	cfg.Auth, err = normalizeTunnelAuth(cfg.Auth)
	if err != nil {
		return ServerConfig{}, err
	}

	cfg.TunnelServer.ListenAddr = strings.TrimSpace(cfg.TunnelServer.ListenAddr)
	if cfg.TunnelServer.ListenAddr == "" {
		cfg.TunnelServer.ListenAddr = DefaultServerListenAddr
	}
	if _, _, err := net.SplitHostPort(cfg.TunnelServer.ListenAddr); err != nil {
		return ServerConfig{}, fmt.Errorf("tunnel_server.listen_addr: %w", err)
	}
	cfg.TunnelServer.OpenTimeout = strings.TrimSpace(cfg.TunnelServer.OpenTimeout)
	if cfg.TunnelServer.OpenTimeout == "" {
		cfg.TunnelServer.OpenTimeout = DefaultOpenTimeout
	}
	if cfg.TunnelServer.TLS.Enabled || cfg.TunnelServer.TLS.CertFile != "" || cfg.TunnelServer.TLS.KeyFile != "" {
		cfg.TunnelServer.TLS.Enabled = true
		cfg.TunnelServer.TLS.CertFile = strings.TrimSpace(cfg.TunnelServer.TLS.CertFile)
		cfg.TunnelServer.TLS.KeyFile = strings.TrimSpace(cfg.TunnelServer.TLS.KeyFile)
		if cfg.TunnelServer.TLS.CertFile == "" || cfg.TunnelServer.TLS.KeyFile == "" {
			return ServerConfig{}, fmt.Errorf("tunnel_server.tls requires cert_file and key_file")
		}
	}
	seenClients := make(map[string]struct{}, len(cfg.TunnelServer.Clients))
	for index, client := range cfg.TunnelServer.Clients {
		client.ClientID = strings.TrimSpace(client.ClientID)
		client.Token = strings.TrimSpace(client.Token)
		if client.ClientID == "" {
			return ServerConfig{}, fmt.Errorf("tunnel_server.clients[%d]: missing client_id", index)
		}
		if client.Token == "" {
			return ServerConfig{}, fmt.Errorf("tunnel_server.clients[%d]: missing token", index)
		}
		if _, ok := seenClients[client.ClientID]; ok {
			return ServerConfig{}, fmt.Errorf("duplicate tunnel client_id: %s", client.ClientID)
		}
		seenClients[client.ClientID] = struct{}{}
		client.Allow = normalizeAllow(client.Allow)
		cfg.TunnelServer.Clients[index] = client
	}
	return cfg, nil
}

func normalizeClientConfig(cfg ClientConfig) (ClientConfig, error) {
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Version != 1 {
		return ClientConfig{}, fmt.Errorf("unsupported config version: %d", cfg.Version)
	}
	client := cfg.TunnelClient
	client.ServerAddr = strings.TrimSpace(client.ServerAddr)
	if client.ServerAddr == "" {
		return ClientConfig{}, fmt.Errorf("tunnel_client.server_addr is required")
	}
	if _, _, err := net.SplitHostPort(client.ServerAddr); err != nil {
		return ClientConfig{}, fmt.Errorf("tunnel_client.server_addr: %w", err)
	}
	client.ClientID = strings.TrimSpace(client.ClientID)
	client.Token = strings.TrimSpace(client.Token)
	if client.ClientID == "" {
		return ClientConfig{}, fmt.Errorf("tunnel_client.client_id is required")
	}
	if client.Token == "" {
		return ClientConfig{}, fmt.Errorf("tunnel_client.token is required")
	}
	client.DialTimeout = strings.TrimSpace(client.DialTimeout)
	if client.DialTimeout == "" {
		client.DialTimeout = DefaultDialTimeout
	}
	client.ReconnectMin = strings.TrimSpace(client.ReconnectMin)
	if client.ReconnectMin == "" {
		client.ReconnectMin = DefaultReconnectMin
	}
	client.ReconnectMax = strings.TrimSpace(client.ReconnectMax)
	if client.ReconnectMax == "" {
		client.ReconnectMax = DefaultReconnectMax
	}
	if client.TLS.ServerName == "" {
		host, _, _ := net.SplitHostPort(client.ServerAddr)
		client.TLS.ServerName = host
	}
	if len(client.Tunnels) == 0 {
		return ClientConfig{}, fmt.Errorf("tunnel_client.tunnels must not be empty")
	}
	seenTunnels := make(map[string]struct{}, len(client.Tunnels))
	seenRemote := make(map[string]string, len(client.Tunnels))
	for index, spec := range client.Tunnels {
		spec = normalizeTunnelSpec(spec)
		if err := validateTunnelSpec(spec); err != nil {
			return ClientConfig{}, fmt.Errorf("tunnel_client.tunnels[%d]: %w", index, err)
		}
		if _, ok := seenTunnels[spec.TunnelID]; ok {
			return ClientConfig{}, fmt.Errorf("duplicate tunnel_id: %s", spec.TunnelID)
		}
		seenTunnels[spec.TunnelID] = struct{}{}
		remoteKey := remoteListenKey(spec)
		if other, ok := seenRemote[remoteKey]; ok {
			return ClientConfig{}, fmt.Errorf("tunnels %s and %s use the same remote listener %s", other, spec.TunnelID, remoteKey)
		}
		seenRemote[remoteKey] = spec.TunnelID
		client.Tunnels[index] = spec
	}
	cfg.TunnelClient = client
	return cfg, nil
}

func normalizeTunnelSpec(spec TunnelSpec) TunnelSpec {
	spec.TunnelID = strings.TrimSpace(spec.TunnelID)
	spec.Protocol = strings.ToLower(strings.TrimSpace(spec.Protocol))
	if spec.Protocol == "" {
		spec.Protocol = "tcp"
	}
	spec.RemoteListenAddr = strings.TrimSpace(spec.RemoteListenAddr)
	if spec.RemoteListenAddr == "" {
		spec.RemoteListenAddr = "0.0.0.0"
	}
	spec.LocalAddr = strings.TrimSpace(spec.LocalAddr)
	if spec.LocalAddr == "" {
		spec.LocalAddr = "127.0.0.1"
	}
	spec.Remark = strings.TrimSpace(spec.Remark)
	return spec
}

func validateTunnelSpec(spec TunnelSpec) error {
	if spec.TunnelID == "" {
		return fmt.Errorf("missing tunnel_id")
	}
	if spec.Protocol != "tcp" && spec.Protocol != "udp" {
		return fmt.Errorf("unsupported protocol: %s", spec.Protocol)
	}
	if spec.RemoteListenPort <= 0 || spec.RemoteListenPort > 65535 {
		return fmt.Errorf("remote_listen_port out of range")
	}
	if spec.LocalAddr == "" {
		return fmt.Errorf("missing local_addr")
	}
	if spec.LocalPort <= 0 || spec.LocalPort > 65535 {
		return fmt.Errorf("local_port out of range")
	}
	if spec.MaxConn < 0 {
		return fmt.Errorf("max_conn must be >= 0")
	}
	if spec.SpeedLimit < 0 {
		return fmt.Errorf("speed_limit must be >= 0")
	}
	return nil
}

func normalizeAllow(allow AllowConfig) AllowConfig {
	for i := range allow.Protocols {
		allow.Protocols[i] = strings.ToLower(strings.TrimSpace(allow.Protocols[i]))
	}
	sort.Strings(allow.Protocols)
	sort.Ints(allow.RemotePorts)
	return allow
}

func remoteListenKey(spec TunnelSpec) string {
	return strings.ToLower(strings.TrimSpace(spec.Protocol)) + ":" + net.JoinHostPort(strings.TrimSpace(spec.RemoteListenAddr), fmt.Sprintf("%d", spec.RemoteListenPort))
}

func normalizeTunnelAuth(auth config.AuthConfig) (config.AuthConfig, error) {
	seen := make(map[string]struct{}, len(auth.Tokens))
	for index, token := range auth.Tokens {
		token.Name = strings.TrimSpace(token.Name)
		token.Token = strings.TrimSpace(token.Token)
		token.Role = strings.ToLower(strings.TrimSpace(token.Role))
		if token.Role == "" {
			token.Role = config.AuthRoleAdmin
		}
		if token.Role != config.AuthRoleAdmin && token.Role != config.AuthRoleViewer {
			return config.AuthConfig{}, fmt.Errorf("auth.tokens[%d]: invalid role: %s", index, token.Role)
		}
		if token.Token == "" {
			if auth.Enabled {
				return config.AuthConfig{}, fmt.Errorf("auth.tokens[%d]: missing token", index)
			}
			auth.Tokens[index] = token
			continue
		}
		if _, ok := seen[token.Token]; ok {
			return config.AuthConfig{}, fmt.Errorf("auth.tokens[%d]: duplicate token", index)
		}
		seen[token.Token] = struct{}{}
		if token.Name == "" {
			token.Name = fmt.Sprintf("token-%d", index+1)
		}
		auth.Tokens[index] = token
	}
	if auth.Enabled && len(seen) == 0 {
		return config.AuthConfig{}, fmt.Errorf("auth enabled but no tokens configured")
	}
	return auth, nil
}
