package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newIntegrationDaemon starts a real control-API daemon backed by a real
// engine manager and an on-disk config file, mirroring the controlapi test
// harness. The returned client speaks to it exactly like production MCP does.
func newIntegrationDaemon(t *testing.T, token string) (*controlapi.Client, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := `# preserve this comment
version: 1
udp_max_sessions: 64
auth:
  enabled: true
  tokens:
    - name: operator
      token: admin-secret
      role: admin
    - name: observer
      token: viewer-secret
      role: viewer
rules:
  - rule_id: old-rule
    name: old rule
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 24001
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &controlapi.Runtime{
		ConfigPath: configPath,
		Manager:    engine.NewManager(engine.NewCollector()),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth: controlapi.NewAuthenticator(config.AuthConfig{
			Enabled: true,
			Tokens: []config.AuthToken{
				{Name: "operator", Token: "admin-secret", Role: config.AuthRoleAdmin},
				{Name: "observer", Token: "viewer-secret", Role: config.AuthRoleViewer},
			},
		}),
	}
	runtime.PrecheckOptions = &precheck.Options{}
	handler := controlapi.NewHandler(runtime)
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		runtime.Manager.StopAll()
	})
	return controlapi.NewClient(server.URL, token), configPath
}

func callWriteTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return nil, nil, err
	}
	got := map[string]any{}
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured content: %v", err)
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal structured content: %v", err)
		}
	}
	return result, got, nil
}

func TestWriteToolsEndToEnd(t *testing.T) {
	client, configPath := newIntegrationDaemon(t, "admin-secret")
	session := connectClient(t, NewServer(client, "vtest", Options{WriteEnabled: true}))

	upsertArgs := func(dryRun bool) map[string]any {
		return map[string]any{
			"rule_id":     "new-rule",
			"name":        "new rule",
			"protocol":    "tcp+udp",
			"listen_addr": "127.0.0.1",
			"listen_port": 24002,
			"target_addr": "127.0.0.1",
			"target_port": 53,
			"enabled":     false,
			"bandwidth_limit": map[string]any{
				"rate": "100 Mbps",
			},
			"traffic_quota": map[string]any{
				"daily":    "50GB",
				"timezone": "UTC",
			},
			"dry_run": dryRun,
		}
	}

	result, got, err := callWriteTool(t, session, toolUpsertForwardingRule, upsertArgs(true))
	if err != nil {
		t.Fatalf("dry-run upsert protocol error: %v", err)
	}
	if result.IsError {
		t.Fatalf("dry-run upsert tool error: %+v", result.Content)
	}
	if got["applied"] != false || got["action"] != "created" {
		t.Fatalf("dry-run output = %v", got)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadIntegrationConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Rules) != 1 || loaded.Rules[0].RuleID != "old-rule" {
		t.Fatalf("dry-run mutated the config file: %+v", loaded.Rules)
	}

	result, got, err = callWriteTool(t, session, toolUpsertForwardingRule, upsertArgs(false))
	if err != nil {
		t.Fatalf("upsert protocol error: %v", err)
	}
	if result.IsError {
		t.Fatalf("upsert tool error: %+v", result.Content)
	}
	if got["applied"] != true || got["action"] != "created" {
		t.Fatalf("upsert output = %v", got)
	}
	loaded, err = loadIntegrationConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Rules) != 2 {
		t.Fatalf("persisted rules = %d, want old + new", len(loaded.Rules))
	}
	var persisted *engine.Rule
	for index := range loaded.Rules {
		if loaded.Rules[index].RuleID == "new-rule" {
			persisted = &loaded.Rules[index]
		}
	}
	if persisted == nil || persisted.Protocol != engine.ProtocolTCPUDP || persisted.Bandwidth == nil || persisted.Bandwidth.Rate != 12_500_000 {
		t.Fatalf("persisted new rule = %+v", persisted)
	}
	if persisted.CreatedTime == 0 || persisted.Revision == 0 {
		t.Fatalf("daemon did not stamp identity fields: %+v", persisted)
	}

	_, got, err = callWriteTool(t, session, toolSetForwardingRuleEnabled, map[string]any{"rule_id": "new-rule", "enabled": true})
	if err != nil || got["action"] != "enabled" || got["applied"] != true {
		t.Fatalf("enable output = %v, err = %v", got, err)
	}
	loaded, err = loadIntegrationConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := range loaded.Rules {
		if loaded.Rules[index].RuleID == "new-rule" && !loaded.Rules[index].Enabled {
			t.Fatalf("enable did not persist: %+v", loaded.Rules[index])
		}
	}

	_, got, err = callWriteTool(t, session, toolDeleteForwardingRule, map[string]any{"rule_id": "new-rule"})
	if err != nil || got["action"] != "deleted" || got["applied"] != true {
		t.Fatalf("delete output = %v, err = %v", got, err)
	}
	loaded, err = loadIntegrationConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Rules) != 1 || loaded.Rules[0].RuleID != "old-rule" {
		t.Fatalf("delete did not persist: %+v", loaded.Rules)
	}
}

func TestWriteToolsViewerTokenIsDenied(t *testing.T) {
	client, _ := newIntegrationDaemon(t, "viewer-secret")
	session := connectClient(t, NewServer(client, "vtest", Options{WriteEnabled: true}))

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: toolUpsertForwardingRule,
		Arguments: map[string]any{
			"rule_id": "denied", "name": "denied", "protocol": "tcp",
			"listen_addr": "127.0.0.1", "listen_port": 24003,
			"target_addr": "127.0.0.1", "target_port": 22,
		},
	})
	if err != nil {
		t.Fatalf("viewer upsert protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("viewer upsert unexpectedly succeeded: %+v", result.Content)
	}
	if !strings.Contains(fmtContent(result), "admin management token") {
		t.Fatalf("viewer error did not explain the role gap: %s", fmtContent(result))
	}

	statusResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolGetVMFlowStatus})
	if err != nil {
		t.Fatalf("status protocol error: %v", err)
	}
	got := map[string]any{}
	data, _ := json.Marshal(statusResult.StructuredContent)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["mcp_read_only"] != false {
		t.Fatalf("write-mode status reported read-only: %v", got)
	}
	issues, _ := got["issues"].([]any)
	found := false
	for _, issue := range issues {
		if strings.Contains(fmt.Sprint(issue), "admin management token") {
			found = true
		}
	}
	if !found {
		t.Fatalf("status issues did not flag the write capability gap: %v", issues)
	}
}

func loadIntegrationConfig(path string) (config.File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config.File{}, err
	}
	return config.Parse(raw)
}

func fmtContent(result *mcp.CallToolResult) string {
	data, err := json.Marshal(result.Content)
	if err != nil {
		return ""
	}
	return string(data)
}
