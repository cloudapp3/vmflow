package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientManagementContract(t *testing.T) {
	t.Helper()
	const etag = `"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Access"); got != "allowed" {
			t.Fatalf("X-Access = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/session":
			writeTestJSON(t, w, http.StatusOK, map[string]any{
				"actor": "operator", "role": "admin",
				"capabilities": map[string]bool{"rules_write": true},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config/rules":
			w.Header().Set("ETag", etag)
			writeTestJSON(t, w, http.StatusOK, map[string]any{
				"revision": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"writable": true, "udp_max_sessions": 256,
				"rules": []map[string]any{{
					"rule_id": "disabled", "name": "disabled", "protocol": "tcp",
					"listen_addr": "127.0.0.1", "listen_port": 2201,
					"target_addr": "127.0.0.1", "target_port": 22, "enabled": false,
					"idle_timeout": 30, "revision": 7,
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/config/rules/precheck":
			if got := r.Header.Get("If-Match"); got != etag {
				t.Fatalf("precheck If-Match = %q", got)
			}
			var request ConfigRulesRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.UDPMaxSessions != 128 || len(request.Rules) != 1 {
				t.Fatalf("precheck request = %+v", request)
			}
			writeTestJSON(t, w, http.StatusUnprocessableEntity, map[string]any{
				"revision":                 "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"udp_max_sessions_changed": true,
				"diff":                     []map[string]string{{"rule_id": "disabled", "config_action": "update", "runtime_action": "none"}},
				"precheck": map[string]any{
					"ok": false, "error_count": 1, "warning_count": 0, "checked_rules": 1,
					"checked_time_ms": 1,
					"items":           []map[string]string{{"severity": "error", "check": "listen_bind", "rule_id": "disabled", "message": "busy"}},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/config/rules":
			if r.Header.Get("If-Match") == `"sha256:stale"` {
				writeTestJSON(t, w, http.StatusPreconditionFailed, map[string]string{"error": "configuration revision changed"})
				return
			}
			if got := r.Header.Get("If-Match"); got != etag {
				t.Fatalf("apply If-Match = %q", got)
			}
			w.Header().Set("ETag", `"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`)
			writeTestJSON(t, w, http.StatusOK, map[string]any{
				"revision":         "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				"udp_max_sessions": 128,
				"rules":            []any{},
				"result":           map[string]any{"applied_rules": 0, "stopped_rules": 1, "failed_rules": 0, "total_rules": 0, "items": []any{}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL+"/", "secret")
	client.SetHeaders(http.Header{"X-Access": []string{"allowed"}})
	ctx := context.Background()

	session, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if session.Actor != "operator" || !session.Capabilities.RulesWrite {
		t.Fatalf("session = %+v", session)
	}
	snapshot, err := client.ConfigRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ETag != etag || len(snapshot.Rules) != 1 || snapshot.Rules[0].Enabled {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Rules[0].IdleTimeout != 30 || snapshot.Rules[0].Revision != 7 {
		t.Fatalf("full rule fields were not decoded: %+v", snapshot.Rules[0])
	}

	draft := ConfigRulesRequest{UDPMaxSessions: 128, Rules: cloneRules(snapshot.Rules)}
	checked, err := client.Precheck(ctx, snapshot.ETag, draft)
	if err != nil {
		t.Fatalf("422 precheck should decode findings: %v", err)
	}
	if checked.Precheck.OK || len(checked.Diff) != 1 || checked.Diff[0].ConfigAction != "update" || !checked.UDPMaxSessionsChanged {
		t.Fatalf("precheck = %+v", checked)
	}

	applied, err := client.Apply(ctx, snapshot.ETag, draft)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision == snapshot.Revision || applied.ETag == "" || applied.Result.StoppedRules != 1 {
		t.Fatalf("apply = %+v", applied)
	}

	_, err = client.Apply(ctx, "sha256:stale", draft)
	if apiStatus(err) != http.StatusPreconditionFailed {
		t.Fatalf("stale status = %d, err = %v", apiStatus(err), err)
	}
}

func TestNormalizeETag(t *testing.T) {
	if got := normalizeETag("sha256:value"); got != `"sha256:value"` {
		t.Fatalf("normalizeETag = %q", got)
	}
	if got := normalizeETag(`"sha256:value"`); got != `"sha256:value"` {
		t.Fatalf("quoted normalizeETag = %q", got)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
