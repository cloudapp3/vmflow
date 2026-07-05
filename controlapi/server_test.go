package controlapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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

	resp, err := http.Get(server.URL + "/healthz")
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

	req, err := http.NewRequest(http.MethodGet, server.URL+"/healthz", nil)
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
}
