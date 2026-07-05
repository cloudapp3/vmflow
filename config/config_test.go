package config

import "testing"

func TestParseDefaultsAdminListenAddr(t *testing.T) {
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
	if cfg.AdminListenAddr != DefaultAdminListenAddr {
		t.Fatalf("expected default admin addr %s, got %s", DefaultAdminListenAddr, cfg.AdminListenAddr)
	}
}

func TestParseRejectsDuplicateRuleID(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
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
version: 1
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
