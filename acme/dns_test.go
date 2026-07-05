package acme

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockDNSProvider is a test double for DNSProvider.
type mockDNSProvider struct {
	records   map[string]string // fqdn -> value
	presented []string
	cleaned   []string
	failClean bool
}

func newMockDNSProvider() *mockDNSProvider {
	return &mockDNSProvider{records: make(map[string]string)}
}

func (m *mockDNSProvider) Present(ctx context.Context, fqdn, value string) error {
	m.records[fqdn] = value
	m.presented = append(m.presented, fqdn)
	return nil
}

func (m *mockDNSProvider) CleanUp(ctx context.Context, fqdn string) error {
	if m.failClean {
		return fmt.Errorf("cleanup failed")
	}
	delete(m.records, fqdn)
	m.cleaned = append(m.cleaned, fqdn)
	return nil
}

func TestDNS01SolverPresentAndCleanUp(t *testing.T) {
	mock := newMockDNSProvider()
	solver := NewDNS01Solver(mock, DNS01Options{})

	ctx := context.Background()
	fqdn := "_acme-challenge.example.com"
	value := "test-token-value"

	if err := solver.Present(ctx, fqdn, value); err != nil {
		t.Fatalf("present: %v", err)
	}

	if mock.records[fqdn] != value {
		t.Fatalf("expected record %s=%s, got %s", fqdn, value, mock.records[fqdn])
	}

	if err := solver.CleanUp(ctx, fqdn); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, ok := mock.records[fqdn]; ok {
		t.Fatal("expected record to be removed after cleanup")
	}
}

func TestDNS01SolverDefaults(t *testing.T) {
	mock := newMockDNSProvider()
	solver := NewDNS01Solver(mock, DNS01Options{})

	if solver.propagationTimeout != 120*time.Second {
		t.Fatalf("expected default propagation timeout 120s, got %s", solver.propagationTimeout)
	}
	if solver.pollingInterval != 5*time.Second {
		t.Fatalf("expected default polling interval 5s, got %s", solver.pollingInterval)
	}
}

func TestDNS01SolverCustomTimeouts(t *testing.T) {
	mock := newMockDNSProvider()
	opts := DNS01Options{
		PropagationTimeout: 60 * time.Second,
		PollingInterval:    2 * time.Second,
	}
	solver := NewDNS01Solver(mock, opts)

	if solver.propagationTimeout != 60*time.Second {
		t.Fatalf("expected 60s, got %s", solver.propagationTimeout)
	}
	if solver.pollingInterval != 2*time.Second {
		t.Fatalf("expected 2s, got %s", solver.pollingInterval)
	}
}

func TestChallengeFQDN(t *testing.T) {
	tests := []struct {
		domain string
		want   string
	}{
		{"example.com", "_acme-challenge.example.com"},
		{"sub.example.com", "_acme-challenge.sub.example.com"},
		{"*.example.com", "_acme-challenge.*.example.com"},
		{"example.com.", "_acme-challenge.example.com"},
	}
	for _, tc := range tests {
		got := ChallengeFQDN(tc.domain)
		if got != tc.want {
			t.Errorf("ChallengeFQDN(%q) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

func TestNewManagerWithDNS(t *testing.T) {
	mock := newMockDNSProvider()
	solver := NewDNS01Solver(mock, DNS01Options{})
	mgr := NewManagerWithDNS(t.TempDir(), solver)

	if mgr.challengeType != "dns-01" {
		t.Fatalf("expected challenge type dns-01, got %s", mgr.challengeType)
	}
	if mgr.dnsSolver == nil {
		t.Fatal("expected dnsSolver to be set")
	}
	if mgr.solver != nil {
		t.Fatal("expected http solver to be nil")
	}
}

func TestNewManagerHTTP01(t *testing.T) {
	solver := NewHTTP01Solver(":80")
	mgr := NewManager(t.TempDir(), solver)

	if mgr.challengeType != "http-01" {
		t.Fatalf("expected challenge type http-01, got %s", mgr.challengeType)
	}
	if mgr.solver == nil {
		t.Fatal("expected http solver to be set")
	}
	if mgr.dnsSolver != nil {
		t.Fatal("expected dnsSolver to be nil")
	}
}

func TestExecProviderPresentAndCleanUp(t *testing.T) {
	// Create a temporary script that records calls
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls.log")

	script := filepath.Join(dir, "dns-hook.sh")
	scriptContent := "#!/bin/sh\necho \"$@\" >> " + logFile + "\nexit 0\n"
	if err := os.WriteFile(script, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	provider := NewExecProvider(script)
	ctx := context.Background()

	if err := provider.Present(ctx, "_acme-challenge.example.com", "token-value"); err != nil {
		t.Fatalf("present: %v", err)
	}

	if err := provider.CleanUp(ctx, "_acme-challenge.example.com"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Verify script was called correctly
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	if !contains(log, "present _acme-challenge.example.com token-value") {
		t.Fatalf("expected present call in log, got: %s", log)
	}
	if !contains(log, "cleanup _acme-challenge.example.com") {
		t.Fatalf("expected cleanup call in log, got: %s", log)
	}
}

func TestExecProviderFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	provider := NewExecProvider(script)
	ctx := context.Background()

	err := provider.Present(ctx, "_acme-challenge.example.com", "token")
	if err == nil {
		t.Fatal("expected error from failing script")
	}
}

func TestExecProviderNotFound(t *testing.T) {
	provider := NewExecProvider("/nonexistent/script.sh")
	ctx := context.Background()

	err := provider.Present(ctx, "_acme-challenge.example.com", "token")
	if err == nil {
		t.Fatal("expected error for nonexistent script")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
