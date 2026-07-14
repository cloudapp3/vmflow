package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/engine"
	"gopkg.in/yaml.v3"
)

const DefaultControlListenAddr = "127.0.0.1:19090"
const DefaultLogLevel = "info"
const DefaultLogFormat = "text"

const (
	AuthRoleAdmin  = "admin"
	AuthRoleViewer = "viewer"
)

type File struct {
	Version           int              `json:"version" yaml:"version"`
	ControlListenAddr string           `json:"control_listen_addr" yaml:"control_listen_addr"`
	UDPMaxSessions    int              `json:"udp_max_sessions,omitempty" yaml:"udp_max_sessions,omitempty"`
	ControlTLS        ControlTLSConfig `json:"control_tls,omitempty" yaml:"control_tls,omitempty"`
	Log               LogConfig        `json:"log,omitempty" yaml:"log,omitempty"`
	Auth              AuthConfig       `json:"auth,omitempty" yaml:"auth,omitempty"`
	BotToken          string           `json:"bot_token,omitempty" yaml:"bot_token,omitempty"`
	BotChat           int64            `json:"bot_chat,omitempty" yaml:"bot_chat,omitempty"`
	BotControlToken   string           `json:"bot_control_token,omitempty" yaml:"bot_control_token,omitempty"`
	AcmeChallenge     string           `json:"acme_challenge,omitempty" yaml:"acme_challenge,omitempty"`
	AcmeHTTP01Addr    string           `json:"acme_http01_addr,omitempty" yaml:"acme_http01_addr,omitempty"`
	AcmeCacheDir      string           `json:"acme_cache_dir,omitempty" yaml:"acme_cache_dir,omitempty"`
	AcmeDNS01         DNS01Config      `json:"acme_dns01,omitempty" yaml:"acme_dns01,omitempty"`
	CertCacheDir      string           `json:"cert_cache_dir,omitempty" yaml:"cert_cache_dir,omitempty"`
	CertReview        CertReviewConfig `json:"cert_review,omitempty" yaml:"cert_review,omitempty"`
	Stats             StatsConfig      `json:"stats,omitempty" yaml:"stats,omitempty"`
	Rules             []engine.Rule    `json:"rules" yaml:"rules"`
}

// StatsConfig controls optional persistence of cumulative traffic counters so
// daemon restarts do not lose per-rule upload/download/drop totals.
type StatsConfig struct {
	Persist       bool   `json:"persist" yaml:"persist"`
	Path          string `json:"path,omitempty" yaml:"path,omitempty"`
	FlushInterval string `json:"flush_interval,omitempty" yaml:"flush_interval,omitempty"`
}

// CertReviewConfig controls certificate review thresholds.
type CertReviewConfig struct {
	ExpiryWarningDays  int `json:"expiry_warning_days,omitempty" yaml:"expiry_warning_days,omitempty"`
	ExpiryCriticalDays int `json:"expiry_critical_days,omitempty" yaml:"expiry_critical_days,omitempty"`
	MinRSABits         int `json:"min_rsa_bits,omitempty" yaml:"min_rsa_bits,omitempty"`
}

// DNS01Config configures DNS-01 ACME challenge solving.
type DNS01Config struct {
	Provider             string `json:"provider,omitempty" yaml:"provider,omitempty"`
	PropagationTimeout   string `json:"propagation_timeout,omitempty" yaml:"propagation_timeout,omitempty"`
	PollingInterval      string `json:"polling_interval,omitempty" yaml:"polling_interval,omitempty"`
	CloudflareAPIToken   string `json:"cloudflare_api_token,omitempty" yaml:"cloudflare_api_token,omitempty"`
	RFC2136Nameserver    string `json:"rfc2136_nameserver,omitempty" yaml:"rfc2136_nameserver,omitempty"`
	RFC2136TSIGAlgorithm string `json:"rfc2136_tsig_algorithm,omitempty" yaml:"rfc2136_tsig_algorithm,omitempty"`
	RFC2136TSIGKey       string `json:"rfc2136_tsig_key,omitempty" yaml:"rfc2136_tsig_key,omitempty"`
	RFC2136TSIGSecret    string `json:"rfc2136_tsig_secret,omitempty" yaml:"rfc2136_tsig_secret,omitempty"`
	ExecPath             string `json:"exec_path,omitempty" yaml:"exec_path,omitempty"`
}

// ControlTLSConfig enables TLS (and optionally mTLS) on the control API.
// TLS is active when both CertFile and KeyFile are set. Setting ClientCAFile
// turns on mutual TLS, requiring every client to present a certificate signed
// by that CA.
type ControlTLSConfig struct {
	CertFile     string `json:"cert_file,omitempty" yaml:"cert_file,omitempty"`
	KeyFile      string `json:"key_file,omitempty" yaml:"key_file,omitempty"`
	ClientCAFile string `json:"client_ca_file,omitempty" yaml:"client_ca_file,omitempty"`
	MinVersion   string `json:"min_version,omitempty" yaml:"min_version,omitempty"`
}

// LogConfig controls structured logging for daemon mode.
type LogConfig struct {
	Level  string `json:"level,omitempty" yaml:"level,omitempty"`
	Format string `json:"format,omitempty" yaml:"format,omitempty"`
}

// AuthConfig controls Control API bearer-token authentication.
type AuthConfig struct {
	Enabled bool        `json:"enabled" yaml:"enabled"`
	Tokens  []AuthToken `json:"tokens,omitempty" yaml:"tokens,omitempty"`
}

// AuthToken is one Control API bearer token. Token values are secrets and must
// not be logged.
type AuthToken struct {
	Name  string `json:"name,omitempty" yaml:"name,omitempty"`
	Token string `json:"token" yaml:"token"`
	Role  string `json:"role,omitempty" yaml:"role,omitempty"`
}

func Load(path string) (File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return File{}, fmt.Errorf("missing config path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (File, error) {
	var cfg File
	if len(raw) == 0 {
		return File{}, fmt.Errorf("empty config")
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return File{}, err
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Version != 1 {
		return File{}, fmt.Errorf("unsupported config version: %d", cfg.Version)
	}
	cfg.ControlListenAddr = strings.TrimSpace(cfg.ControlListenAddr)
	if cfg.ControlListenAddr == "" {
		cfg.ControlListenAddr = DefaultControlListenAddr
	}
	if cfg.UDPMaxSessions == 0 {
		cfg.UDPMaxSessions = engine.DefaultUDPGlobalMaxSessions
	}
	if cfg.UDPMaxSessions < 0 || cfg.UDPMaxSessions > engine.MaxUDPGlobalMaxSessions {
		return File{}, fmt.Errorf("udp_max_sessions must be 0 (default) or between 1 and %d", engine.MaxUDPGlobalMaxSessions)
	}
	cfg.Log = normalizeLog(cfg.Log)
	normalizedStats, err := normalizeStats(cfg.Stats)
	if err != nil {
		return File{}, err
	}
	cfg.Stats = normalizedStats
	cfg.ControlTLS = normalizeControlTLS(cfg.ControlTLS)
	if err := validateControlTLS(cfg.ControlTLS); err != nil {
		return File{}, err
	}
	cfg.Auth, err = normalizeAuth(cfg.Auth)
	if err != nil {
		return File{}, err
	}
	seen := make(map[string]struct{}, len(cfg.Rules))
	for index, rule := range cfg.Rules {
		rule = rule.Standardize()
		if err := rule.Validate(); err != nil {
			return File{}, fmt.Errorf("rules[%d]: %w", index, err)
		}
		if _, ok := seen[rule.RuleID]; ok {
			return File{}, fmt.Errorf("duplicate rule id: %s", rule.RuleID)
		}
		seen[rule.RuleID] = struct{}{}
		cfg.Rules[index] = rule
	}
	return cfg, nil
}

func normalizeStats(stats StatsConfig) (StatsConfig, error) {
	stats.Path = strings.TrimSpace(stats.Path)
	stats.FlushInterval = strings.TrimSpace(stats.FlushInterval)
	if stats.FlushInterval == "" {
		return stats, nil
	}
	interval, err := time.ParseDuration(stats.FlushInterval)
	if err != nil || interval < time.Second {
		return StatsConfig{}, fmt.Errorf("stats.flush_interval must be a duration of at least 1s")
	}
	return stats, nil
}

func normalizeControlTLS(t ControlTLSConfig) ControlTLSConfig {
	t.CertFile = strings.TrimSpace(t.CertFile)
	t.KeyFile = strings.TrimSpace(t.KeyFile)
	t.ClientCAFile = strings.TrimSpace(t.ClientCAFile)
	t.MinVersion = strings.TrimSpace(t.MinVersion)
	return t
}

func validateControlTLS(t ControlTLSConfig) error {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("control_tls: cert_file and key_file must both be set or both omitted")
	}
	if t.ClientCAFile != "" && (t.CertFile == "" || t.KeyFile == "") {
		return fmt.Errorf("control_tls: client_ca_file requires cert_file and key_file")
	}
	switch t.MinVersion {
	case "", "1.2", "1.3":
	default:
		return fmt.Errorf("control_tls: min_version must be \"1.2\" or \"1.3\", got %q", t.MinVersion)
	}
	return nil
}

func normalizeLog(logCfg LogConfig) LogConfig {
	logCfg.Level = strings.ToLower(strings.TrimSpace(logCfg.Level))
	if logCfg.Level == "" {
		logCfg.Level = DefaultLogLevel
	}
	logCfg.Format = strings.ToLower(strings.TrimSpace(logCfg.Format))
	if logCfg.Format == "" {
		logCfg.Format = DefaultLogFormat
	}
	return logCfg
}

func normalizeAuth(auth AuthConfig) (AuthConfig, error) {
	seen := make(map[string]struct{}, len(auth.Tokens))
	for index, token := range auth.Tokens {
		token.Name = strings.TrimSpace(token.Name)
		token.Token = strings.TrimSpace(token.Token)
		token.Role = strings.ToLower(strings.TrimSpace(token.Role))
		if token.Role == "" {
			token.Role = AuthRoleAdmin
		}
		if token.Role != AuthRoleAdmin && token.Role != AuthRoleViewer {
			return AuthConfig{}, fmt.Errorf("auth.tokens[%d]: invalid role: %s", index, token.Role)
		}
		if token.Token == "" {
			if auth.Enabled {
				return AuthConfig{}, fmt.Errorf("auth.tokens[%d]: missing token", index)
			}
			auth.Tokens[index] = token
			continue
		}
		if _, ok := seen[token.Token]; ok {
			return AuthConfig{}, fmt.Errorf("auth.tokens[%d]: duplicate token", index)
		}
		seen[token.Token] = struct{}{}
		if token.Name == "" {
			token.Name = fmt.Sprintf("token-%d", index+1)
		}
		auth.Tokens[index] = token
	}
	if auth.Enabled && len(seen) == 0 {
		return AuthConfig{}, fmt.Errorf("auth enabled but no tokens configured")
	}
	return auth, nil
}
