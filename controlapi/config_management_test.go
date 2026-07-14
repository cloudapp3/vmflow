package controlapi

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

func TestSessionReportsReadOnlyWithoutAuthentication(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	NewHandler(testRuntime(config.AuthConfig{})).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("session status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"actor": "anonymous"`) ||
		!strings.Contains(recorder.Body.String(), `"rules_write": false`) {
		t.Fatalf("unexpected session response: %s", recorder.Body.String())
	}
}

func TestSessionReportsAdminAndViewerCapabilities(t *testing.T) {
	authConfig := config.AuthConfig{
		Enabled: true,
		Tokens: []config.AuthToken{
			{Name: "operator", Token: "admin-secret", Role: config.AuthRoleAdmin},
			{Name: "observer", Token: "viewer-secret", Role: config.AuthRoleViewer},
		},
	}
	handler := NewHandler(testRuntime(authConfig))

	for _, test := range []struct {
		name     string
		token    string
		writable bool
	}{
		{name: "admin", token: "admin-secret", writable: true},
		{name: "viewer", token: "viewer-secret", writable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
			request.Header.Set("Authorization", "Bearer "+test.token)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("session status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			want := `"rules_write": ` + map[bool]string{true: "true", false: "false"}[test.writable]
			if !strings.Contains(recorder.Body.String(), want) {
				t.Fatalf("session response missing %q: %s", want, recorder.Body.String())
			}
		})
	}
}

func TestConfigWriteRequiresEnabledAuthentication(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/v1/config/rules", strings.NewReader(`{"udp_max_sessions":256,"rules":[]}`))
	NewHandler(testRuntime(config.AuthConfig{})).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("anonymous configuration write status = %d, want 403: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRulesConfigSnapshotIncludesDisabledRulesWithoutSecrets(t *testing.T) {
	runtime, handler, _ := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()

	recorder := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("config snapshot status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("ETag") == "" {
		t.Fatal("config snapshot omitted ETag")
	}
	body := recorder.Body.String()
	for _, want := range []string{`"writable": true`, `"rule_id": "old-rule"`, `"enabled": false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("config snapshot omitted %s: %s", want, body)
		}
	}
	if strings.Contains(body, "admin-secret") || strings.Contains(body, "keep-me-secret") {
		t.Fatalf("config snapshot leaked secret configuration: %s", body)
	}
}

func TestRulesConfigPrecheckDoesNotPersistDraft(t *testing.T) {
	runtime, handler, configPath := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()
	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	body := `{"udp_max_sessions":128,"rules":[{"rule_id":"new-rule","name":"new rule","protocol":"tcp","listen_addr":"127.0.0.1","listen_port":24001,"target_addr":"127.0.0.1","target_port":22,"enabled":false}]}`
	prechecked := authenticatedManagementRequest(t, handler, http.MethodPost, "/v1/config/rules/precheck", body, get.Header().Get("ETag"))
	if prechecked.Code != http.StatusOK {
		t.Fatalf("precheck status = %d, want 200; body=%s", prechecked.Code, prechecked.Body.String())
	}
	if !strings.Contains(prechecked.Body.String(), `"config_action": "add"`) || !strings.Contains(prechecked.Body.String(), `"config_action": "delete"`) {
		t.Fatalf("precheck response omitted expected diff: %s", prechecked.Body.String())
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Rules) != 1 || loaded.Rules[0].RuleID != "old-rule" || loaded.UDPMaxSessions != 64 {
		t.Fatalf("precheck persisted draft unexpectedly: %+v", loaded)
	}
}

func TestRulesConfigPutPersistsAndReturnsNewRevision(t *testing.T) {
	runtime, handler, configPath := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()
	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	body := `{"udp_max_sessions":128,"rules":[{"rule_id":"new-rule","name":"new rule","protocol":"udp","listen_addr":"127.0.0.1","listen_port":24002,"target_addr":"127.0.0.1","target_port":53,"enabled":false,"remark":"managed"}]}`
	put := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", body, get.Header().Get("ETag"))
	if put.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200; body=%s", put.Code, put.Body.String())
	}
	if put.Header().Get("ETag") == "" || put.Header().Get("ETag") == get.Header().Get("ETag") {
		t.Fatalf("put ETag = %q, old = %q", put.Header().Get("ETag"), get.Header().Get("ETag"))
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UDPMaxSessions != 128 || len(loaded.Rules) != 1 || loaded.Rules[0].RuleID != "new-rule" {
		t.Fatalf("persisted configuration = %+v", loaded)
	}
	if loaded.Rules[0].Revision != 1 || loaded.Rules[0].CreatedTime == 0 || loaded.Rules[0].UpdatedTime == 0 {
		t.Fatalf("server metadata was not assigned: %+v", loaded.Rules[0])
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "custom_unknown: preserved") || !strings.Contains(string(raw), "# preserve this comment") {
		t.Fatalf("managed write discarded unmanaged YAML content:\n%s", raw)
	}
}

func TestRulesConfigPutRejectsStaleRevision(t *testing.T) {
	runtime, handler, configPath := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()
	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	if err := os.WriteFile(configPath, []byte(managementConfig("external-rule")), 0o600); err != nil {
		t.Fatal(err)
	}
	put := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", `{"udp_max_sessions":128,"rules":[]}`, get.Header().Get("ETag"))
	if put.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale put status = %d, want 412; body=%s", put.Code, put.Body.String())
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Rules) != 1 || loaded.Rules[0].RuleID != "external-rule" {
		t.Fatalf("stale put overwrote external configuration: %+v", loaded.Rules)
	}
}

func TestRulesConfigPutRejectsViewerAndInvalidBody(t *testing.T) {
	runtime, handler, _ := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()

	viewer := httptest.NewRequest(http.MethodPut, "/v1/config/rules", strings.NewReader(`{"udp_max_sessions":64,"rules":[]}`))
	viewer.Header.Set("Authorization", "Bearer viewer-secret")
	viewer.Header.Set("If-Match", `"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`)
	viewerRecorder := httptest.NewRecorder()
	handler.ServeHTTP(viewerRecorder, viewer)
	if viewerRecorder.Code != http.StatusForbidden {
		t.Fatalf("viewer put status = %d, want 403", viewerRecorder.Code)
	}

	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	invalid := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", `{"udp_max_sessions":64,"rules":[],"auth":{}}`, get.Header().Get("ETag"))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid put status = %d, want 400; body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestRulesConfigPutRequiresValidIfMatch(t *testing.T) {
	runtime, handler, _ := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()
	body := `{"udp_max_sessions":64,"rules":[]}`

	missing := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", body, "")
	if missing.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status = %d, want 428; body=%s", missing.Code, missing.Body.String())
	}
	malformed := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", body, `"sha256:not-a-hash"`)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed If-Match status = %d, want 400; body=%s", malformed.Code, malformed.Body.String())
	}
}

func TestRulesConfigPutRejectsInvalidDraftWithoutChangingFile(t *testing.T) {
	runtime, handler, configPath := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	body := `{"udp_max_sessions":64,"rules":[{"rule_id":"bad","name":"bad","protocol":"tcp","listen_addr":"127.0.0.1","listen_port":70000,"target_addr":"127.0.0.1","target_port":22,"enabled":true}]}`
	put := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", body, get.Header().Get("ETag"))
	if put.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid draft status = %d, want 422; body=%s", put.Code, put.Body.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("invalid draft changed config:\n%s", after)
	}
}

func TestRulesConfigPutApplyFailureRestoresRuntimeAndDisk(t *testing.T) {
	oldProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldPort := oldProbe.Addr().(*net.TCPAddr).Port
	_ = oldProbe.Close()
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	blockedPort := blocked.Addr().(*net.TCPAddr).Port

	runtime, handler, configPath := newManagementTestHandler(t, managementEnabledConfig(oldPort))
	defer runtime.Manager.StopAll()
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	initial := runtime.Manager.ApplySnapshotTransactional(loaded.Rules, engine.ApplySnapshotOptions{ReplaceAll: true})
	if initial.ApplyFailure != nil {
		t.Fatalf("start initial runtime: %+v", initial)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	body := fmt.Sprintf(`{"udp_max_sessions":64,"rules":[{"rule_id":"live","name":"live","protocol":"tcp","listen_addr":"127.0.0.1","listen_port":%d,"target_addr":"127.0.0.1","target_port":22,"enabled":true}]}`, blockedPort)
	put := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", body, get.Header().Get("ETag"))
	if put.Code != http.StatusInternalServerError {
		t.Fatalf("failed apply status = %d, want 500; body=%s", put.Code, put.Body.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed apply changed disk configuration:\n%s", after)
	}
	running := runtime.Manager.RunningRules()
	if len(running) != 1 || running[0].RuleID != "live" || running[0].ListenPort != oldPort {
		t.Fatalf("runtime was not restored after failed apply: %+v", running)
	}
	if degraded, reason := runtime.degradedState(); degraded {
		t.Fatalf("successful rollback marked runtime degraded: %s", reason)
	}
}

func TestRulesConfigCommitConflictRollsBackSameRuntimeTransaction(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenPort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	runtime, handler, configPath := newManagementTestHandler(t, managementRollbackConfig(listenPort))
	collector := engine.NewCollector()
	runtime.Manager = engine.NewManager(collector)
	defer runtime.Manager.StopAll()
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if result := runtime.Manager.ApplySnapshotTransactional(loaded.Rules, engine.ApplySnapshotOptions{ReplaceAll: true}); result.ApplyFailure != nil {
		t.Fatalf("initial apply = %+v", result)
	}
	collector.AddUpload("live", 33)
	external := append([]byte(managementRollbackConfig(listenPort)), []byte("# external edit\n")...)
	runtime.configHooks = &configManagementHooks{BeforeCommit: func(*stagedConfig) {
		if err := os.WriteFile(configPath, external, 0o600); err != nil {
			t.Errorf("external edit: %v", err)
		}
	}}

	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	put := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", `{"udp_max_sessions":64,"rules":[]}`, get.Header().Get("ETag"))
	if put.Code != http.StatusPreconditionFailed {
		t.Fatalf("commit conflict status = %d, want 412; body=%s", put.Code, put.Body.String())
	}
	running := runtime.Manager.RunningRules()
	if len(running) != 1 || running[0].RuleID != "live" || running[0].ListenPort != listenPort {
		t.Fatalf("runtime was not restored: %+v", running)
	}
	if got := runtime.Manager.Snapshot("live"); got.UploadBytes != 33 || got.Conns != 0 {
		t.Fatalf("live counter was not restored: %+v", got)
	}
	if protocol := runtime.Manager.RuleProtocols()["disabled"]; protocol != engine.ProtocolUDP {
		t.Fatalf("disabled rule protocol was not restored: %q", protocol)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(external) {
		t.Fatalf("commit conflict overwrote external edit:\n%s", after)
	}
}

func TestRulesConfigPostRenameDurabilityFailureKeepsCommittedRuntime(t *testing.T) {
	runtime, handler, configPath := newManagementTestHandler(t, managementConfig("old-rule"))
	defer runtime.Manager.StopAll()
	runtime.configHooks = &configManagementHooks{BeforeCommit: func(staged *stagedConfig) {
		staged.syncDirectory = func(string) error { return errors.New("injected directory sync failure") }
	}}
	get := authenticatedManagementRequest(t, handler, http.MethodGet, "/v1/config/rules", "", "")
	body := `{"udp_max_sessions":128,"rules":[{"rule_id":"committed","name":"committed","protocol":"udp","listen_addr":"127.0.0.1","listen_port":24003,"target_addr":"127.0.0.1","target_port":53,"enabled":false}]}`
	put := authenticatedManagementRequest(t, handler, http.MethodPut, "/v1/config/rules", body, get.Header().Get("ETag"))
	if put.Code != http.StatusServiceUnavailable || !strings.Contains(put.Body.String(), `"committed": true`) {
		t.Fatalf("durability failure status = %d, want committed 503; body=%s", put.Code, put.Body.String())
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UDPMaxSessions != 128 || len(loaded.Rules) != 1 || loaded.Rules[0].RuleID != "committed" {
		t.Fatalf("committed config was rolled back: %+v", loaded)
	}
	if protocol := runtime.Manager.RuleProtocols()["committed"]; protocol != engine.ProtocolUDP {
		t.Fatalf("committed runtime was rolled back: protocol=%q", protocol)
	}
	if degraded, reason := runtime.degradedState(); !degraded || !strings.Contains(reason, "durability") {
		t.Fatalf("durability failure degraded state = (%v, %q)", degraded, reason)
	}
}

func newManagementTestHandler(t *testing.T, raw string) (*Runtime, http.Handler, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	authConfig := config.AuthConfig{
		Enabled: true,
		Tokens: []config.AuthToken{
			{Name: "operator", Token: "admin-secret", Role: config.AuthRoleAdmin},
			{Name: "observer", Token: "viewer-secret", Role: config.AuthRoleViewer},
		},
	}
	runtime := testRuntime(authConfig)
	runtime.ConfigPath = configPath
	options := precheck.Options{}
	runtime.PrecheckOptions = &options
	return runtime, NewHandler(runtime), configPath
}

func authenticatedManagementRequest(t *testing.T, handler http.Handler, method, path, body, etag string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer admin-secret")
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func managementConfig(ruleID string) string {
	return fmt.Sprintf(`# preserve this comment
version: 1
udp_max_sessions: 64
custom_unknown: preserved
auth:
  enabled: true
  tokens:
    - name: operator
      token: admin-secret
      role: admin
    - name: observer
      token: viewer-secret
      role: viewer
acme_dns01:
  cloudflare_api_token: keep-me-secret
rules:
  - rule_id: %s
    name: old rule
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 24000
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
`, ruleID)
}

func managementEnabledConfig(listenPort int) string {
	return fmt.Sprintf(`version: 1
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
  - rule_id: live
    name: live
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: %d
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
`, listenPort)
}

func managementRollbackConfig(listenPort int) string {
	return fmt.Sprintf(`version: 1
udp_max_sessions: 64
auth:
  enabled: true
  tokens:
    - name: operator
      token: admin-secret
      role: admin
rules:
  - rule_id: live
    name: live
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: %d
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
  - rule_id: disabled
    name: disabled
    protocol: udp
    listen_addr: 127.0.0.1
    listen_port: 24004
    target_addr: 127.0.0.1
    target_port: 53
    enabled: false
`, listenPort)
}
