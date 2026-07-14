package controlapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudapp3/vmflow/config"
)

func TestIPLimiterLocksAfterThreshold(t *testing.T) {
	l := newIPLimiter()
	for i := 0; i < authFailMax; i++ {
		if l.locked("1.2.3.4") {
			t.Fatalf("locked before threshold at attempt %d", i)
		}
		l.note("1.2.3.4", false)
	}
	if !l.locked("1.2.3.4") {
		t.Fatal("expected lockout after reaching threshold")
	}
}

func TestIPLimiterSuccessResets(t *testing.T) {
	l := newIPLimiter()
	for i := 0; i < authFailMax-1; i++ {
		l.note("1.2.3.4", false)
	}
	l.note("1.2.3.4", true) // success clears the counter
	for i := 0; i < authFailMax-1; i++ {
		l.note("1.2.3.4", false)
	}
	if l.locked("1.2.3.4") {
		t.Fatal("success should have reset the counter")
	}
}

func TestIPLimiterIsolatedPerIP(t *testing.T) {
	l := newIPLimiter()
	for i := 0; i < authFailMax; i++ {
		l.note("1.1.1.1", false)
	}
	if !l.locked("1.1.1.1") {
		t.Fatal("1.1.1.1 should be locked")
	}
	if l.locked("2.2.2.2") {
		t.Fatal("2.2.2.2 should not be affected by 1.1.1.1 failures")
	}
}

// TestAuthRateLimitedReturns429 exercises the middleware wiring: after enough
// failed auth attempts from one peer, the next request is rejected with 429.
func TestAuthRateLimitedReturns429(t *testing.T) {
	runtime := testRuntime(config.AuthConfig{
		Enabled: true,
		Tokens:  []config.AuthToken{{Name: "admin", Token: "secret", Role: config.AuthRoleAdmin}},
	})
	server := httptest.NewServer(NewHandler(runtime))
	defer server.Close()

	for i := 0; i < authFailMax; i++ {
		resp, err := http.Get(server.URL + "/v1/rules")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, resp.StatusCode)
		}
	}

	resp, err := http.Get(server.URL + "/v1/rules")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after threshold, got %d", resp.StatusCode)
	}
}
