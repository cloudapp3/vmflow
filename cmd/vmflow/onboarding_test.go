package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

func TestInspectStatusReportsConfigAndRunningDaemon(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nrules:\n  - rule_id: one\n    name: one\n    protocol: tcp\n    listen_addr: 127.0.0.1\n    listen_port: 2201\n    target_addr: 127.0.0.1\n    target_port: 22\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &controlapi.Runtime{
		ConfigPath: configPath, ServerVersion: "v-test", StartedAt: time.Now(),
		Manager: engine.NewManager(engine.NewCollector()),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth:    controlapi.NewAuthenticator(config.AuthConfig{}),
	}
	server := httptest.NewServer(controlapi.NewHandler(runtime))
	defer server.Close()
	report := inspectStatus(statusInspectOptions{ConfigPath: configPath, Address: server.URL, Timeout: time.Second})
	if report.Status != "running" || report.DaemonVersion != "v-test" || report.ConfiguredRules != 1 || report.EnabledRules != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestPrintGuideProvidesStateSpecificNextStep(t *testing.T) {
	var output bytes.Buffer
	printGuide(&output, statusReport{Version: "dev", Status: "not running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded", ConfiguredRules: 1})
	if text := output.String(); !strings.Contains(text, "vmflow init") || !strings.Contains(text, "0 enabled") {
		t.Fatalf("zero-rule guide:\n%s", text)
	}

	output.Reset()
	printGuide(&output, statusReport{Version: "dev", Status: "running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded", ConfiguredRules: 1, EnabledRules: 1})
	if text := output.String(); !strings.Contains(text, "vmflow tui") || strings.Contains(text, "vmflow run -config") {
		t.Fatalf("running guide:\n%s", text)
	}
}

func TestPrintRuntimeReadyShowsActionableState(t *testing.T) {
	var output bytes.Buffer
	printRuntimeReady(&output, runtimeReadyInfo{
		ConfigPath: "/opt/vmflow/config.yaml", ControlAddress: "http://127.0.0.1:19090", ConfiguredRules: 1,
	})
	text := output.String()
	if !strings.Contains(text, "No forwarding rules are active") || !strings.Contains(text, "vmflow init") || !strings.Contains(text, "Ctrl+C") {
		t.Fatalf("inactive summary:\n%s", text)
	}

	output.Reset()
	printRuntimeReady(&output, runtimeReadyInfo{
		ConfigPath: "/opt/vmflow/config.yaml", ControlAddress: "http://127.0.0.1:19090", ConfiguredRules: 1, EnabledRules: 1, ActiveRules: 1,
	})
	text = output.String()
	if !strings.Contains(text, "1 active / 1 enabled") || !strings.Contains(text, "vmflow tui") {
		t.Fatalf("active summary:\n%s", text)
	}
}

func TestProbeDaemonDistinguishesAuthentication(t *testing.T) {
	runtime := &controlapi.Runtime{
		Manager: engine.NewManager(engine.NewCollector()),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth: controlapi.NewAuthenticator(config.AuthConfig{Enabled: true, Tokens: []config.AuthToken{{
			Name: "admin", Token: "secret", Role: config.AuthRoleAdmin,
		}}}),
	}
	server := httptest.NewServer(controlapi.NewHandler(runtime))
	defer server.Close()
	if got := probeDaemon(server.URL, "", controlapi.ClientTLSOptions{}, nil, time.Second); got.Status != "running" || !strings.Contains(got.Detail, "authentication") {
		t.Fatalf("unauthenticated probe = %+v", got)
	}
	if got := probeDaemon(server.URL, "secret", controlapi.ClientTLSOptions{}, nil, time.Second); got.Status != "running" {
		t.Fatalf("authenticated probe = %+v", got)
	}
}

func TestManagementPortOpenRejectsInvalidAddress(t *testing.T) {
	if open, err := managementPortOpen(":bad", time.Millisecond); err == nil || open {
		t.Fatalf("managementPortOpen = %v, %v", open, err)
	}
}
