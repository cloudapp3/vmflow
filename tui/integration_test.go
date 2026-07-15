package tui

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

func TestClientAgainstControlAPIManagementHandler(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte(`version: 1
control_port: 19090
udp_max_sessions: 256
auth:
  enabled: true
  tokens:
    - name: admin
      token: admin-secret
      role: admin
    - name: viewer
      token: viewer-secret
      role: viewer
rules:
  - rule_id: disabled
    name: disabled
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
`)
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager := engine.NewManager(engine.NewCollector())
	defer manager.StopAll()
	checkOptions := precheck.Options{}
	runtime := &controlapi.Runtime{
		ConfigPath:      configPath,
		Manager:         manager,
		Auth:            controlapi.NewAuthenticator(cfg.Auth),
		StartupConfig:   &cfg,
		PrecheckOptions: &checkOptions,
	}
	server := httptest.NewServer(controlapi.NewHandler(runtime))
	defer server.Close()

	ctx := context.Background()
	admin := NewClient(server.URL, "admin-secret")
	session, err := admin.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if session.Role != "admin" || !session.Capabilities.RulesWrite {
		t.Fatalf("admin session = %+v", session)
	}
	snapshot, err := admin.ConfigRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Writable || snapshot.ETag == "" || len(snapshot.Rules) != 1 || snapshot.Rules[0].Enabled {
		t.Fatalf("initial snapshot = %+v", snapshot)
	}

	draftRules := cloneRules(snapshot.Rules)
	draftRules = append(draftRules, RuleInfo{
		RuleID: "second", Name: "second", Protocol: engine.ProtocolUDP,
		ListenAddr: "127.0.0.1", ListenPort: 2202,
		TargetAddr: "127.0.0.1", TargetPort: 53, Enabled: false,
	})
	draft := ConfigRulesRequest{UDPMaxSessions: 128, Rules: draftRules}
	checked, err := admin.Precheck(ctx, snapshot.ETag, draft)
	if err != nil {
		t.Fatal(err)
	}
	if !checked.Precheck.OK || len(checked.Diff) != 1 || checked.Diff[0].RuleID != "second" || !checked.UDPMaxSessionsChanged {
		t.Fatalf("precheck = %+v", checked)
	}
	applied, err := admin.Apply(ctx, snapshot.ETag, draft)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision == snapshot.Revision || len(applied.Rules) != 2 || applied.UDPMaxSessions != 128 {
		t.Fatalf("apply = %+v", applied)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UDPMaxSessions != 128 || len(loaded.Rules) != 2 {
		t.Fatalf("persisted config = %+v", loaded)
	}

	viewer := NewClient(server.URL, "viewer-secret")
	viewerSession, err := viewer.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if viewerSession.Capabilities.RulesWrite {
		t.Fatalf("viewer session = %+v", viewerSession)
	}
	if _, err := viewer.Precheck(ctx, applied.ETag, draft); apiStatus(err) != 403 {
		t.Fatalf("viewer precheck err = %v", err)
	}
}
