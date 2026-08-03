package main

import (
	"bytes"
	"encoding/json"
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
	"github.com/cloudapp3/vmflow/internal/i18n"
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
	printGuide(&output, statusReport{
		Version: "dev", Status: "not running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded",
		ConfiguredRules: 1, BundledExample: true, ConfigIsDefault: true,
	})
	if text := output.String(); !strings.Contains(text, "1. vmflow init") ||
		!strings.Contains(text, "installed example is disabled") ||
		!strings.Contains(text, "In another terminal, run: vmflow tui") ||
		strings.Contains(text, "vmflow init -config") {
		t.Fatalf("zero-rule guide:\n%s", text)
	}

	output.Reset()
	printGuide(&output, statusReport{
		Version: "dev", Status: "running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded",
		ConfiguredRules: 1, EnabledRules: 1, ConfigIsDefault: true,
	})
	if text := output.String(); !strings.Contains(text, "vmflow tui") || strings.Contains(text, "vmflow run -config") {
		t.Fatalf("running guide:\n%s", text)
	}

	output.Reset()
	printGuide(&output, statusReport{
		Version: "dev", Status: "not running", ConfigPath: "/srv/vmflow/config.yaml", ConfigState: "loaded",
		ConfiguredRules: 1, EnabledRules: 1,
	})
	if text := output.String(); !strings.Contains(text, `vmflow run -config "/srv/vmflow/config.yaml"`) ||
		!strings.Contains(text, `vmflow service install --config "/srv/vmflow/config.yaml"`) {
		t.Fatalf("custom-config guide:\n%s", text)
	}

	output.Reset()
	printGuide(&output, statusReport{
		Version: "dev", Status: "running", ConfigPath: "/tmp/config.yaml", ConfigState: "invalid",
		ConfigError: "bad yaml", ConfigIsDefault: true,
	})
	if text := output.String(); !strings.Contains(text, "Running.") ||
		!strings.Contains(text, "configuration on disk is invalid") || strings.Contains(text, "Cannot start") {
		t.Fatalf("running-invalid-config guide:\n%s", text)
	}
}

func TestPrintGuideIncludesProjectAndCommunityLinks(t *testing.T) {
	reports := []statusReport{
		{Version: "dev", Status: "running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded", ConfiguredRules: 1, EnabledRules: 1},
		{Version: "dev", Status: "not running", ConfigPath: "/tmp/config.yaml", ConfigState: "invalid", ConfigError: "bad yaml"},
		{Version: "dev", Status: "unknown", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded", ConfiguredRules: 1, EnabledRules: 1},
		{Version: "dev", Status: "not running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded", ConfiguredRules: 1, BundledExample: true},
		{Version: "dev", Status: "not running", ConfigPath: "/tmp/config.yaml", ConfigState: "loaded", ConfiguredRules: 1, EnabledRules: 1},
	}

	for i, report := range reports {
		var output bytes.Buffer
		printGuide(&output, report)
		text := output.String()
		for _, want := range []string{
			"Commands:  vmflow help",
			"GitHub:    https://github.com/cloudapp3/vmflow",
			"Telegram:  https://t.me/VMPulse",
		} {
			if count := strings.Count(text, want); count != 1 {
				t.Fatalf("report %d contains %q %d times, want once:\n%s", i, want, count, text)
			}
		}
	}
}

func TestPrintGuideIsLocalizedInChinese(t *testing.T) {
	var output bytes.Buffer
	printGuideWithTranslator(&output, statusReport{
		Version: "dev", Status: "not running", ConfigPath: "/opt/vmflow/config.yaml", ConfigState: "loaded",
		ConfiguredRules: 1, BundledExample: true,
	}, i18n.New(i18n.SimplifiedChinese))

	text := output.String()
	for _, want := range []string{
		"尚未运行",
		"安装示例已禁用",
		"首次设置",
		"创建第一条转发规则",
		"vmflow init -config \"/opt/vmflow/config.yaml\"",
		"vmflow tui",
		projectGitHubURL,
		communityTelegramURL,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Chinese guide does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "cli.") || strings.Contains(text, "First-time setup") {
		t.Fatalf("Chinese guide leaked a key or English guide heading:\n%s", text)
	}
}

func TestStatusJSONIsLanguageInvariant(t *testing.T) {
	report := statusReport{
		Name: "vmflow", Version: "v-test", Status: "not running", StatusDetail: "connection refused",
		ConfigPath: "/opt/vmflow/config.yaml", ConfigState: "loaded", ConfiguredRules: 2, EnabledRules: 1,
		ControlAddress: "http://127.0.0.1:19090", ServiceState: "not installed",
	}
	var english, chinese bytes.Buffer
	if err := writeStatusReport(&english, report, true, i18n.New(i18n.English)); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusReport(&chinese, report, true, i18n.New(i18n.SimplifiedChinese)); err != nil {
		t.Fatal(err)
	}
	if english.String() != chinese.String() {
		t.Fatalf("status JSON changed with locale\nEnglish:\n%s\nChinese:\n%s", english.String(), chinese.String())
	}
	var decoded statusReport
	if err := json.Unmarshal(chinese.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status != "not running" || decoded.ConfigState != "loaded" || decoded.ServiceState != "not installed" {
		t.Fatalf("status protocol values were localized: %+v", decoded)
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
	if !strings.Contains(text, "1 active / 1 enabled") || !strings.Contains(text, "vmflow tui") ||
		!strings.Contains(text, "press Ctrl+C") || !strings.Contains(text, "vmflow service install --config '/opt/vmflow/config.yaml'") {
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
