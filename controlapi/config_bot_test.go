package controlapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/config"
)

// fakeBotController records Apply/Stop calls without touching Telegram.
type fakeBotController struct {
	mu       sync.Mutex
	applied  []BotSettings
	stops    int
	running  bool
	applyErr error
}

type blockingBotController struct {
	mu           sync.Mutex
	applied      []BotSettings
	running      bool
	firstStarted chan struct{}
	releaseFirst chan struct{}
	once         sync.Once
}

func (controller *blockingBotController) Apply(settings BotSettings) error {
	controller.mu.Lock()
	controller.applied = append(controller.applied, settings)
	first := len(controller.applied) == 1
	controller.mu.Unlock()
	if first {
		controller.once.Do(func() { close(controller.firstStarted) })
		<-controller.releaseFirst
	}
	controller.mu.Lock()
	controller.running = settings.Token != "" && settings.ChatID != 0
	controller.mu.Unlock()
	return nil
}

func (controller *blockingBotController) Stop() error {
	controller.mu.Lock()
	controller.running = false
	controller.mu.Unlock()
	return nil
}

func (controller *blockingBotController) Running() bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.running
}

func (controller *blockingBotController) appliedSettings() []BotSettings {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return append([]BotSettings(nil), controller.applied...)
}

func (f *fakeBotController) Apply(s BotSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, s)
	if f.applyErr != nil {
		return f.applyErr
	}
	f.running = s.Token != "" && s.ChatID != 0
	return nil
}

func (f *fakeBotController) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.running = false
	return nil
}

func (f *fakeBotController) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func botConfigYAML(token string, chat int64, control string) string {
	return fmt.Sprintf(`version: 1
control_listen_addr: 127.0.0.1:19090
auth:
  enabled: true
  tokens:
    - name: admin
      token: secret
      role: admin
bot_token: %q
bot_chat: %d
bot_control_token: %q
rules: []
`, token, chat, control)
}

func botTestRuntime(t *testing.T, raw string, bot BotController) *Runtime {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rt := testRuntime(config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "admin", Token: "secret", Role: config.AuthRoleAdmin}},
	})
	rt.ConfigPath = configPath
	rt.Bot = bot
	return rt
}

func TestGetBotConfig(t *testing.T) {
	fake := &fakeBotController{running: true}
	rt := botTestRuntime(t, botConfigYAML("123:abc", 111, "ctrl"), fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	resp, err := client.BotConfig(context.Background())
	if err != nil {
		t.Fatalf("BotConfig: %v", err)
	}
	if resp.BotToken != "123:abc" || resp.BotChat != 111 || resp.BotControlToken != "ctrl" || !resp.Running {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPutBotConfigWritesAndApplies(t *testing.T) {
	fake := &fakeBotController{}
	rt := botTestRuntime(t, botConfigYAML("123:abc", 111, "ctrl"), fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	cfg, err := client.BotConfig(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp, err := client.ApplyBotConfig(context.Background(), cfg.ETag, BotConfigRequest{
		BotToken: "999:xyz", BotChat: 222, BotControlToken: "new-ctrl",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if resp.BotToken != "999:xyz" || resp.BotChat != 222 {
		t.Fatalf("response: %+v", resp)
	}
	if len(fake.applied) != 1 || fake.applied[0].Token != "999:xyz" || fake.applied[0].ChatID != 222 || fake.applied[0].ControlToken != "new-ctrl" {
		t.Fatalf("Apply not called with new settings: %+v", fake.applied)
	}
	raw, err := os.ReadFile(rt.ConfigPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), "999:xyz") || !strings.Contains(string(raw), "222") {
		t.Fatalf("config not persisted: %s", raw)
	}
}

func TestPutBotConfigReadinessFailureLeavesConfigAndRunningBotUntouched(t *testing.T) {
	fake := &fakeBotController{
		running:  true,
		applyErr: &BotValidationError{Err: errors.New("rejected")},
	}
	original := botConfigYAML("123:abc", 111, "ctrl")
	rt := botTestRuntime(t, original, fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	cfg, err := client.BotConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ApplyBotConfig(context.Background(), cfg.ETag, BotConfigRequest{
		BotToken: "invalid", BotChat: 222, BotControlToken: "new-ctrl",
	})
	if APIStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("apply status = %d, want 422; err=%v", APIStatus(err), err)
	}
	raw, readErr := os.ReadFile(rt.ConfigPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != original {
		t.Fatalf("failed readiness changed config:\n%s", raw)
	}
	if !fake.Running() {
		t.Fatal("failed readiness stopped the previous bot")
	}
}

func TestPutBotConfigNormalizesWhitespaceTokenToDisabled(t *testing.T) {
	fake := &fakeBotController{running: true}
	rt := botTestRuntime(t, botConfigYAML("123:abc", 111, "ctrl"), fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	cfg, err := client.BotConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.ApplyBotConfig(context.Background(), cfg.ETag, BotConfigRequest{
		BotToken: " \t ", BotChat: 222, BotControlToken: " control ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.BotToken != "" || resp.BotControlToken != "control" || resp.Running {
		t.Fatalf("normalized response = %+v", resp)
	}
	persisted, err := config.Load(rt.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.BotToken != "" || persisted.BotControlToken != "control" {
		t.Fatalf("persisted bot settings = (%q, %q)", persisted.BotToken, persisted.BotControlToken)
	}
}

func TestPutBotConfigCommitConflictRestoresPreviousRuntime(t *testing.T) {
	fake := &fakeBotController{running: true}
	original := botConfigYAML("123:abc", 111, "ctrl")
	external := botConfigYAML("external:token", 333, "external-control")
	rt := botTestRuntime(t, original, fake)
	rt.configHooks = &configManagementHooks{BeforeCommit: func(*stagedConfig) {
		if err := os.WriteFile(rt.ConfigPath, []byte(external), 0o600); err != nil {
			t.Fatalf("external write: %v", err)
		}
	}}
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	cfg, err := client.BotConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ApplyBotConfig(context.Background(), cfg.ETag, BotConfigRequest{
		BotToken: "999:xyz", BotChat: 222, BotControlToken: "new-control",
	})
	if APIStatus(err) != http.StatusPreconditionFailed {
		t.Fatalf("apply status = %d, want 412; err=%v", APIStatus(err), err)
	}
	if got := fake.applied; len(got) != 2 || got[0].Token != "999:xyz" || got[1].Token != "123:abc" {
		t.Fatalf("runtime was not rolled back: %+v", got)
	}
	raw, readErr := os.ReadFile(rt.ConfigPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != external {
		t.Fatalf("external config was overwritten:\n%s", raw)
	}
	if !fake.Running() {
		t.Fatal("previous running state was not restored")
	}
}

func TestPutBotConfigConflict(t *testing.T) {
	fake := &fakeBotController{}
	rt := botTestRuntime(t, botConfigYAML("123:abc", 111, ""), fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	stale := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	_, err := client.ApplyBotConfig(context.Background(), stale, BotConfigRequest{BotToken: "x", BotChat: 1})
	if APIStatus(err) != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %v", err)
	}
	if len(fake.applied) != 0 {
		t.Fatalf("Apply should not run on conflict: %+v", fake.applied)
	}
}

func TestStartStopBot(t *testing.T) {
	fake := &fakeBotController{}
	rt := botTestRuntime(t, botConfigYAML("123:abc", 111, ""), fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	if err := client.StartBot(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(fake.applied) != 1 || fake.applied[0].Token != "123:abc" || fake.applied[0].ChatID != 111 {
		t.Fatalf("start should apply config settings: %+v", fake.applied)
	}
	if err := client.StopBot(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if fake.stops != 1 {
		t.Fatalf("stops = %d", fake.stops)
	}
}

func TestBotStartSerializesWithConfigPut(t *testing.T) {
	fake := &blockingBotController{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	rt := botTestRuntime(t, botConfigYAML("123:abc", 111, "ctrl"), fake)
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()
	client := NewClient(server.URL, "secret")
	cfg, err := client.BotConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	startDone := make(chan error, 1)
	go func() { startDone <- client.StartBot(context.Background()) }()
	select {
	case <-fake.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("start did not reach Apply")
	}
	putDone := make(chan error, 1)
	go func() {
		_, applyErr := client.ApplyBotConfig(context.Background(), cfg.ETag, BotConfigRequest{
			BotToken: "999:xyz", BotChat: 222, BotControlToken: "new-control",
		})
		putDone <- applyErr
	}()
	select {
	case err := <-putDone:
		t.Fatalf("PUT bypassed lifecycle transaction lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(fake.releaseFirst)
	if err := <-startDone; err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := <-putDone; err != nil {
		t.Fatalf("put: %v", err)
	}
	applied := fake.appliedSettings()
	if len(applied) != 2 || applied[0].Token != "123:abc" || applied[1].Token != "999:xyz" {
		t.Fatalf("apply order = %+v", applied)
	}
}

func TestReloadRejectsManualBotChangesButAcceptsManagedBotUpdate(t *testing.T) {
	originalRaw := botConfigYAML("123:abc", 111, "ctrl")
	originalConfig, err := config.Parse([]byte(originalRaw))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("manual change requires restart", func(t *testing.T) {
		fake := &fakeBotController{running: true}
		rt := botTestRuntime(t, originalRaw, fake)
		rt.StartupConfig = &originalConfig
		if err := os.WriteFile(rt.ConfigPath, []byte(botConfigYAML("manual:new", 222, "manual-control")), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, reloadErr := rt.Reload()
		var restartErr *RestartRequiredError
		if !errors.As(reloadErr, &restartErr) {
			t.Fatalf("reload error = %v, want RestartRequiredError", reloadErr)
		}
		joined := strings.Join(restartErr.Fields, ",")
		for _, field := range []string{"bot_token", "bot_chat", "bot_control_token"} {
			if !strings.Contains(joined, field) {
				t.Fatalf("restart fields = %v, missing %s", restartErr.Fields, field)
			}
		}
	})

	t.Run("managed update advances reload baseline", func(t *testing.T) {
		fake := &fakeBotController{running: true}
		rt := botTestRuntime(t, originalRaw, fake)
		startup := originalConfig
		rt.StartupConfig = &startup
		server := httptest.NewServer(NewHandler(rt))
		defer server.Close()
		client := NewClient(server.URL, "secret")
		cfg, getErr := client.BotConfig(context.Background())
		if getErr != nil {
			t.Fatal(getErr)
		}
		if _, applyErr := client.ApplyBotConfig(context.Background(), cfg.ETag, BotConfigRequest{
			BotToken: "999:xyz", BotChat: 222, BotControlToken: "new-control",
		}); applyErr != nil {
			t.Fatal(applyErr)
		}
		if _, _, reloadErr := rt.Reload(); reloadErr != nil {
			t.Fatalf("reload after managed bot update: %v", reloadErr)
		}
	})
}

func TestGetBotConfigViewerForbidden(t *testing.T) {
	// getBotConfig uses authorizeWrite (admin gate), not authorizeConfigWrite,
	// so an unauthenticated loopback session can still read bot state for
	// diagnosis; only viewer-role tokens are forbidden from reading the bot token.
	rt := testRuntime(config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "viewer", Token: "v", Role: config.AuthRoleViewer}},
	})
	rt.Bot = &fakeBotController{}
	server := httptest.NewServer(NewHandler(rt))
	defer server.Close()

	client := NewClient(server.URL, "v")
	_, err := client.BotConfig(context.Background())
	if APIStatus(err) != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer, got %v", err)
	}
}
