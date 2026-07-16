package controlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

func TestAuthRequired(t *testing.T) {
	runtime := testRuntime(config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "admin", Token: "secret", Role: config.AuthRoleAdmin}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/rules")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestViewerCannotReload(t *testing.T) {
	runtime := testRuntime(config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "viewer", Token: "view", Role: config.AuthRoleViewer}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/reload", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer view")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminTokenCanRead(t *testing.T) {
	runtime := testRuntime(config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "admin", Token: "secret", Role: config.AuthRoleAdmin}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/rules", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func testRuntime(authCfg config.AuthConfig) *Runtime {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Runtime{
		ConfigPath: "missing.yaml",
		Manager:    engine.NewManager(engine.NewCollector()),
		Logger:     logger,
		Auth:       NewAuthenticator(authCfg),
	}
}

func TestBearerPrefixCaseInsensitive(t *testing.T) {
	auth := NewAuthenticator(config.AuthConfig{Enabled: true, Tokens: []config.AuthToken{{Token: "secret"}}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer secret")
	info, ok := auth.Authenticate(req)
	if !ok {
		t.Fatal("expected auth ok")
	}
	if strings.TrimSpace(info.Role) != RoleAdmin {
		t.Fatalf("expected admin role, got %q", info.Role)
	}
}

func TestPrecheckEndpoint(t *testing.T) {
	tmp := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(tmp, []byte(`
version: 1
rules:
  - rule_id: r1
    name: r1
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
`), 0644); err != nil {
		t.Fatal(err)
	}

	runtime := testRuntime(config.AuthConfig{})
	runtime.ConfigPath = tmp
	opts := precheck.Options{CheckTargetResolve: true}
	runtime.PrecheckOptions = &opts
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/precheck", "application/json", nil)
	if err != nil {
		t.Fatalf("precheck: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var response CurrentPrecheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode precheck response: %v", err)
	}
	if !response.Result.OK || response.RuleCount != 1 || response.ConfigPath != "config.yaml" {
		t.Fatalf("precheck response = %+v", response)
	}
}

func TestPrecheckEndpointReportsConfigLoadFailureAsFinding(t *testing.T) {
	runtime := testRuntime(config.AuthConfig{})
	runtime.ConfigPath = t.TempDir() + "/missing.yaml"

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/precheck", nil)
	NewHandler(runtime).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("precheck status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	var response CurrentPrecheckResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode precheck response: %v", err)
	}
	if response.Error != "configuration could not be loaded" || response.Result.OK || response.Result.ErrorCount != 1 {
		t.Fatalf("precheck response = %+v", response)
	}
	if len(response.Result.Items) != 1 || response.Result.Items[0].Check != "config_load" {
		t.Fatalf("precheck findings = %+v", response.Result.Items)
	}
}

func TestReloadAppliesUDPMaxSessions(t *testing.T) {
	tmp := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(tmp, []byte(`
version: 1
udp_max_sessions: 1234
rules: []
`), 0644); err != nil {
		t.Fatal(err)
	}

	runtime := testRuntime(config.AuthConfig{})
	startup, err := config.Parse([]byte("version: 1\nrules: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.StartupConfig = &startup
	runtime.ConfigPath = tmp
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts
	defer runtime.Manager.StopAll()

	if _, _, err := runtime.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	limit, active := runtime.Manager.UDPMaxSessions()
	if limit != 1234 || active != 0 {
		t.Fatalf("UDPMaxSessions() = (%d, %d), want (1234, 0)", limit, active)
	}
}

func TestReloadRejectsRestartOnlyConfigChanges(t *testing.T) {
	startup, err := config.Parse([]byte(`
version: 1
auth:
  enabled: true
  tokens:
    - name: admin
      token: old-secret
      role: admin
rules: []
`))
	if err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte(`
version: 1
auth:
  enabled: true
  tokens:
    - name: admin
      token: new-secret
      role: admin
rules: []
`), 0644); err != nil {
		t.Fatal(err)
	}

	runtime := testRuntime(config.AuthConfig{})
	runtime.StartupConfig = &startup
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts

	_, _, err = runtime.Reload()
	var restartErr *RestartRequiredError
	if !errors.As(err, &restartErr) {
		t.Fatalf("reload error = %v, want RestartRequiredError", err)
	}
	if len(restartErr.Fields) != 1 || restartErr.Fields[0] != "auth" {
		t.Fatalf("restart-required fields = %v, want [auth]", restartErr.Fields)
	}
}

func TestReloadRejectsStatsConfigChanges(t *testing.T) {
	startup, err := config.Parse([]byte("version: 1\nrules: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte("version: 1\nstats:\n  persist: true\n  flush_interval: 30s\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(config.AuthConfig{})
	runtime.StartupConfig = &startup
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts

	_, _, err = runtime.Reload()
	var restartErr *RestartRequiredError
	if !errors.As(err, &restartErr) {
		t.Fatalf("reload error = %v, want RestartRequiredError", err)
	}
	if len(restartErr.Fields) != 1 || restartErr.Fields[0] != "stats" {
		t.Fatalf("restart-required fields = %v, want [stats]", restartErr.Fields)
	}
}

func TestReloadRejectsControlPortChanges(t *testing.T) {
	startup, err := config.Parse([]byte("version: 1\ncontrol_port: 19090\nrules: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte("version: 1\ncontrol_port: 19091\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(config.AuthConfig{})
	runtime.StartupConfig = &startup
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts

	_, _, err = runtime.Reload()
	var restartErr *RestartRequiredError
	if !errors.As(err, &restartErr) {
		t.Fatalf("reload error = %v, want RestartRequiredError", err)
	}
	if len(restartErr.Fields) != 1 || restartErr.Fields[0] != "control_port" {
		t.Fatalf("restart-required fields = %v, want [control_port]", restartErr.Fields)
	}
}

func TestReloadEndpointReturnsConflictForRestartOnlyChanges(t *testing.T) {
	startup, err := config.Parse([]byte("version: 1\nrules: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte("version: 1\nlog:\n  format: json\nrules: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runtime := testRuntime(config.AuthConfig{})
	runtime.StartupConfig = &startup
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/reload", nil)
	NewHandler(runtime).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("reload status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"fields": [`) || !strings.Contains(recorder.Body.String(), `"log"`) {
		t.Fatalf("reload response omitted restart-required field: %s", recorder.Body.String())
	}
}

func TestReloadTightensUDPMaxSessions(t *testing.T) {
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte("version: 1\nudp_max_sessions: 64\nrules: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(config.AuthConfig{})
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts

	if _, _, err := runtime.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	limit, active := runtime.Manager.UDPMaxSessions()
	if limit != 64 || active != 0 {
		t.Fatalf("UDPMaxSessions() = (%d, %d), want (64, 0)", limit, active)
	}
}

func TestReloadSerializesTransactions(t *testing.T) {
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte("version: 1\nrules: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(config.AuthConfig{})
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts

	runtime.reloadMu.Lock()
	locked := true
	defer func() {
		if locked {
			runtime.reloadMu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, _, err := runtime.Reload()
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("reload bypassed transaction lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	runtime.reloadMu.Unlock()
	locked = false

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reload after unlock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not resume after transaction lock was released")
	}
}

func TestReloadRuleFailureReturnsErrorAndDoesNotRelaxUDPMaxSessions(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	configPath := t.TempDir() + "/config.yaml"
	raw := fmt.Sprintf(`
version: 1
udp_max_sessions: 1234
rules:
  - rule_id: blocked
    name: blocked
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: %d
    target_addr: 127.0.0.1
    target_port: 9
    enabled: true
`, port)
	if err := os.WriteFile(configPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	runtime := testRuntime(config.AuthConfig{})
	runtime.Manager.SetUDPMaxSessions(64)
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts
	defer runtime.Manager.StopAll()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/reload", nil)
	NewHandler(runtime).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("reload status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"failed_rules": 1`) {
		t.Fatalf("reload response omitted apply result: %s", recorder.Body.String())
	}
	limit, active := runtime.Manager.UDPMaxSessions()
	if limit != 64 || active != 0 {
		t.Fatalf("UDPMaxSessions() = (%d, %d), want unchanged (64, 0)", limit, active)
	}
}

func TestReloadRuleFailureRestoresTightenedUDPMaxSessions(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	configPath := t.TempDir() + "/config.yaml"
	raw := fmt.Sprintf(`
version: 1
udp_max_sessions: 32
rules:
  - rule_id: blocked
    name: blocked
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: %d
    target_addr: 127.0.0.1
    target_port: 9
    enabled: true
`, port)
	if err := os.WriteFile(configPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	runtime := testRuntime(config.AuthConfig{})
	runtime.Manager.SetUDPMaxSessions(64)
	runtime.ConfigPath = configPath
	opts := precheck.Options{}
	runtime.PrecheckOptions = &opts
	defer runtime.Manager.StopAll()

	_, _, err = runtime.Reload()
	if err == nil {
		t.Fatal("expected reload failure")
	}
	if degraded, reason := runtime.degradedState(); !degraded || !strings.Contains(reason, "not applied") {
		t.Fatalf("failed reload degraded state = (%v, %q), want configuration drift", degraded, reason)
	}
	limit, active := runtime.Manager.UDPMaxSessions()
	if limit != 64 || active != 0 {
		t.Fatalf("UDPMaxSessions() = (%d, %d), want restored (64, 0)", limit, active)
	}
}
