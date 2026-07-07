package tunnel

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
)

func TestTunnelControlHandlerHealthAndMetrics(t *testing.T) {
	server := NewServer(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := NewControlHandler(server, ControlHandlerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health json: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("health ok = %v", body["ok"])
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vmflow_tunnel_clients") {
		t.Fatalf("metrics missing vmflow_tunnel_clients: %s", rec.Body.String())
	}
}

func TestTunnelControlHandlerAuth(t *testing.T) {
	server := NewServer(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	auth := controlapi.NewAuthenticator(config.AuthConfig{Enabled: true, Tokens: []config.AuthToken{{Name: "viewer", Token: "secret", Role: config.AuthRoleViewer}}})
	handler := NewControlHandler(server, ControlHandlerOptions{Auth: auth, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	req := httptest.NewRequest(http.MethodGet, "/v1/tunnel/clients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/tunnel/clients", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func performControlRequest(handler http.Handler, method string, path string, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
