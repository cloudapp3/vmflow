//go:build certs

// Cert API tests are gated behind the "certs" build tag because the cert/ACME
// feature is disabled in the default build (see engine/rule.go Validate and
// controlapi/server.go route registration). Run with: go test -tags certs ./controlapi/

package controlapi

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/certstore"
	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
)

func generateTestCert(t *testing.T, domain string) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	return certPEM, keyPEM
}

func testRuntimeWithCertStore(t *testing.T, authCfg config.AuthConfig) *Runtime {
	t.Helper()
	dir := t.TempDir()
	store := certstore.New(nil, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Runtime{
		ConfigPath: "missing.yaml",
		Manager:    engine.NewManager(engine.NewCollector()),
		Logger:     logger,
		Auth:       NewAuthenticator(authCfg),
		CertStore:  store,
	}
}

func doAuthRequest(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestCertsListEmpty(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs", "", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, ok := result["items"].([]any)
	if !ok {
		t.Fatal("expected items array")
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestCertsImportAndGet(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	certPEM, keyPEM := generateTestCert(t, "test.example.com")

	// Import
	body := fmt.Sprintf(`{"domain":"test.example.com","cert":%q,"key":%q}`, certPEM, keyPEM)
	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs", "", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, b)
	}

	// Get single cert
	resp = doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs/test.example.com", "", "")
	b := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(b), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	certObj, ok := result["cert"].(map[string]any)
	if !ok {
		t.Fatal("expected cert object")
	}
	if certObj["domain"] != "test.example.com" {
		t.Fatalf("expected domain=test.example.com, got %v", certObj["domain"])
	}
	if certObj["source"] != "manual" {
		t.Fatalf("expected source=manual, got %v", certObj["source"])
	}

	// List should contain the cert
	resp = doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs", "", "")
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	if err := json.Unmarshal([]byte(b), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items := result["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
}

func TestCertsImportInvalidPEM(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	body := `{"domain":"bad.com","cert":"not-a-cert","key":"not-a-key"}`
	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs", "", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
}

func TestCertsImportMissingFields(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs", "", `{"domain":"","cert":"x","key":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty domain, got %d", resp.StatusCode)
	}

	resp = doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs", "", `{"domain":"a.com","cert":"","key":""}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty cert/key, got %d", resp.StatusCode)
	}
}

func TestCertsDelete(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	certPEM, keyPEM := generateTestCert(t, "delete.example.com")

	// Import first
	body := fmt.Sprintf(`{"domain":"delete.example.com","cert":%q,"key":%q}`, certPEM, keyPEM)
	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs", "", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import failed: %d", resp.StatusCode)
	}

	// Delete
	resp = doAuthRequest(t, http.MethodDelete, server.URL+"/v1/certs/delete.example.com", "", "")
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	// Verify gone
	resp = doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs/delete.example.com", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestCertsDeleteNotFound(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodDelete, server.URL+"/v1/certs/nonexistent.com", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCertsObtainNoACME(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	body := `{"domains":["example.com"]}`
	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs/obtain", "", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 500, got %d: %s", resp.StatusCode, b)
	}
}

func TestCertsViewerCannotImport(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "viewer", Token: "view123", Role: config.AuthRoleViewer}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	body := `{"domain":"test.com","cert":"x","key":"x"}`
	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs", "view123", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestCertsViewerCannotDelete(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "viewer", Token: "view123", Role: config.AuthRoleViewer}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodDelete, server.URL+"/v1/certs/test.com", "view123", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestCertsViewerCannotObtain(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "viewer", Token: "view123", Role: config.AuthRoleViewer}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	body := `{"domains":["test.com"]}`
	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs/obtain", "view123", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestCertsViewerCanList(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "viewer", Token: "view123", Role: config.AuthRoleViewer}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs", "view123", "")
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestCertsStoreNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runtime := &Runtime{
		ConfigPath: "missing.yaml",
		Manager:    engine.NewManager(engine.NewCollector()),
		Logger:     logger,
		Auth:       NewAuthenticator(config.AuthConfig{}),
		CertStore:  nil,
	}
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	resp = doAuthRequest(t, http.MethodGet, server.URL+"/v1/certs/test.com", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

func TestCertsObtainMissingDomains(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodPost, server.URL+"/v1/certs/obtain", "", `{"domains":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCertsMethodNotAllowed(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	resp := doAuthRequest(t, http.MethodPut, server.URL+"/v1/certs", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}

	resp = doAuthRequest(t, http.MethodPatch, server.URL+"/v1/certs/test.com", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestDaysUntilExpiry(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", -1},
		{"not-a-time", -1},
		{time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339), 29}, // approximately 29-30 days
		{time.Now().Add(-1 * time.Hour).Format(time.RFC3339), 0},       // expired
	}
	for _, tc := range tests {
		got := daysUntilExpiry(tc.input)
		if tc.want >= 0 && got != tc.want {
			// For the "30 days" case, allow ±1 day tolerance
			if tc.want == 29 && (got == 29 || got == 30) {
				continue
			}
			t.Errorf("daysUntilExpiry(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestCertsImportWithBodyReader(t *testing.T) {
	runtime := testRuntimeWithCertStore(t, config.AuthConfig{})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	certPEM, keyPEM := generateTestCert(t, "body-test.com")
	body := fmt.Sprintf(`{"domain":"body-test.com","cert":%q,"key":%q}`, certPEM, keyPEM)

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/certs", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, b)
	}
}
