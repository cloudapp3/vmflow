package config

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
)

func TestBundledConfigStartsWithoutForwarding(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "examples", "config.yaml"))
	if err != nil {
		t.Fatalf("load bundled config: %v", err)
	}
	if len(cfg.Rules) == 0 {
		t.Fatal("bundled config should retain a discoverable example rule")
	}
	for _, rule := range cfg.Rules {
		if rule.Enabled {
			t.Fatalf("bundled rule %q is enabled", rule.RuleID)
		}
		if rule.ListenAddr != "127.0.0.1" {
			t.Fatalf("bundled rule %q listens on %q, want loopback", rule.RuleID, rule.ListenAddr)
		}
	}
}

func TestParseDefaultsControlPort(t *testing.T) {
	cfg, err := Parse([]byte(`
rules:
  - rule_id: r1
    name: r1
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("expected version 1, got %d", cfg.Version)
	}
	if cfg.ControlPort != DefaultControlPort {
		t.Fatalf("expected default control port %d, got %d", DefaultControlPort, cfg.ControlPort)
	}
	if cfg.ControlListenAddress() != DefaultControlListenAddr {
		t.Fatalf("expected default control addr %s, got %s", DefaultControlListenAddr, cfg.ControlListenAddress())
	}
	if cfg.UDPMaxSessions != engine.DefaultUDPGlobalMaxSessions {
		t.Fatalf("expected default UDP session limit %d, got %d", engine.DefaultUDPGlobalMaxSessions, cfg.UDPMaxSessions)
	}
}

func TestParseRejectsUnsupportedVersion(t *testing.T) {
	if _, err := Parse([]byte("version: 99\nrules: []\n")); err == nil || !strings.Contains(err.Error(), "unsupported config version") {
		t.Fatalf("expected unsupported version error, got %v", err)
	}
}

func TestParseControlPort(t *testing.T) {
	cfg, err := Parse([]byte("control_port: 19123\nrules: []\n"))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.ControlPort != 19123 || cfg.ControlListenAddress() != "127.0.0.1:19123" {
		t.Fatalf("unexpected control listener: port=%d addr=%q", cfg.ControlPort, cfg.ControlListenAddress())
	}
}

func TestParseRejectsInvalidControlPort(t *testing.T) {
	for _, value := range []string{"-1", "65536"} {
		if _, err := Parse([]byte("control_port: " + value + "\nrules: []\n")); err == nil || !strings.Contains(err.Error(), "control_port") {
			t.Fatalf("control_port=%s error = %v, want validation error", value, err)
		}
	}
}

func TestParseMigratesLoopbackControlListenAddr(t *testing.T) {
	tests := []struct {
		addr string
		port int
	}{
		{addr: "127.0.0.1:19123", port: 19123},
		{addr: "localhost:19124", port: 19124},
		{addr: "[::1]:19125", port: 19125},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			cfg, err := Parse([]byte("control_listen_addr: '" + tt.addr + "'\nrules: []\n"))
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if !cfg.UsedDeprecatedControlListenAddr {
				t.Fatal("legacy field usage was not recorded")
			}
			if cfg.ControlPort != tt.port || cfg.ControlListenAddress() != "127.0.0.1:"+strconv.Itoa(tt.port) {
				t.Fatalf("legacy address was not mapped to fixed loopback: %q", cfg.ControlListenAddress())
			}
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "control_listen_addr") {
				t.Fatalf("legacy field leaked into JSON: %s", encoded)
			}
		})
	}
}

func TestParseRejectsRemoteOrConflictingLegacyControlListenAddr(t *testing.T) {
	tests := []string{
		"control_listen_addr: 0.0.0.0:19090\nrules: []\n",
		"control_listen_addr: ':19090'\nrules: []\n",
		"control_port: 19090\ncontrol_listen_addr: 127.0.0.1:19090\nrules: []\n",
	}
	for _, raw := range tests {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("Parse(%q) succeeded, want rejection", raw)
		}
	}
}

func TestParseUDPMaxSessions(t *testing.T) {
	cfg, err := Parse([]byte("udp_max_sessions: 2048\nrules: []\n"))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.UDPMaxSessions != 2048 {
		t.Fatalf("udp_max_sessions = %d, want 2048", cfg.UDPMaxSessions)
	}
}

func TestParseRejectsInvalidUDPMaxSessions(t *testing.T) {
	for _, value := range []string{"-1", "4097"} {
		if _, err := Parse([]byte("udp_max_sessions: " + value + "\nrules: []\n")); err == nil {
			t.Fatalf("expected udp_max_sessions=%s to be rejected", value)
		}
	}
}

func TestParseRejectsDuplicateRuleID(t *testing.T) {
	_, err := Parse([]byte(`
rules:
  - rule_id: dup
    name: a
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
  - rule_id: dup
    name: b
    protocol: udp
    listen_addr: 127.0.0.1
    listen_port: 2202
    target_addr: 127.0.0.1
    target_port: 53
    enabled: true
`))
	if err == nil {
		t.Fatal("expected duplicate rule id error")
	}
}

func TestParseAuthAndLogDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
log:
  format: json
auth:
  enabled: true
  tokens:
    - name: admin
      token: secret
      role: admin
rules:
  - rule_id: r1
    name: r1
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.Log.Level != DefaultLogLevel || cfg.Log.Format != "json" {
		t.Fatalf("unexpected log config: %+v", cfg.Log)
	}
	if !cfg.Auth.Enabled || len(cfg.Auth.Tokens) != 1 || cfg.Auth.Tokens[0].Role != AuthRoleAdmin {
		t.Fatalf("unexpected auth config: %+v", cfg.Auth)
	}
}

func TestParseRejectsAuthEnabledWithoutToken(t *testing.T) {
	_, err := Parse([]byte(`
auth:
  enabled: true
rules: []
`))
	if err == nil {
		t.Fatal("expected auth token error")
	}
}

func TestParseRejectsInvalidAuthRole(t *testing.T) {
	_, err := Parse([]byte(`
auth:
  enabled: true
  tokens:
    - token: secret
      role: owner
rules: []
`))
	if err == nil {
		t.Fatal("expected invalid role error")
	}
}

func TestParseRejectsClientCAWithoutServerKeyPair(t *testing.T) {
	_, err := Parse([]byte(`
control_tls:
  client_ca_file: clients-ca.crt
rules: []
`))
	if err == nil {
		t.Fatal("expected client CA without server cert and key to be rejected")
	}
}

func TestParseBotControlToken(t *testing.T) {
	cfg, err := Parse([]byte(`
bot_token: "123:abc"
bot_chat: 111
bot_control_token: "admin-secret"
rules: []
`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.BotToken != "123:abc" || cfg.BotChat != 111 || cfg.BotControlToken != "admin-secret" {
		t.Fatalf("bot fields = %q %d %q", cfg.BotToken, cfg.BotChat, cfg.BotControlToken)
	}
}

func TestParseStatsConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
stats:
  persist: true
  path: " /var/lib/vmflow/stats.json "
  flush_interval: " 30s "
rules: []
`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !cfg.Stats.Persist || cfg.Stats.Path != "/var/lib/vmflow/stats.json" || cfg.Stats.FlushInterval != "30s" {
		t.Fatalf("stats config = %+v", cfg.Stats)
	}
}

func TestParseRejectsInvalidStatsFlushInterval(t *testing.T) {
	for _, interval := range []string{"invalid", "500ms", "0s", "-1s"} {
		t.Run(interval, func(t *testing.T) {
			_, err := Parse([]byte("version: 1\nstats:\n  flush_interval: " + interval + "\nrules: []\n"))
			if err == nil {
				t.Fatalf("flush interval %q should fail", interval)
			}
		})
	}
}
