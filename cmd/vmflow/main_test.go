package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/config"
)

func TestRouteCLI(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		wantCommand string
		wantArgs    []string
	}{
		{name: "empty starts foreground", wantCommand: "foreground"},
		{name: "runtime flags start foreground", input: []string{"-config", "/tmp/config.yaml"}, wantCommand: "foreground", wantArgs: []string{"-config", "/tmp/config.yaml"}},
		{name: "top-level help", input: []string{"--help"}, wantCommand: "help"},
		{name: "ctl", input: []string{"ctl", "rules"}, wantCommand: "ctl", wantArgs: []string{"rules"}},
		{name: "ctl alias", input: []string{"c", "rules"}, wantCommand: "c", wantArgs: []string{"rules"}},
		{name: "tui", input: []string{"tui", "-addr", "http://localhost"}, wantCommand: "tui", wantArgs: []string{"-addr", "http://localhost"}},
		{name: "version", input: []string{"version", "-json"}, wantCommand: "version", wantArgs: []string{"-json"}},
		{name: "update", input: []string{"update", "--check"}, wantCommand: "update", wantArgs: []string{"--check"}},
		{name: "service", input: []string{"service", "status"}, wantCommand: "service", wantArgs: []string{"status"}},
		{name: "uninstall", input: []string{"uninstall", "--dry-run"}, wantCommand: "uninstall", wantArgs: []string{"--dry-run"}},
		{name: "removed daemon command", input: []string{"daemon", "-config", "config.yaml"}, wantCommand: "unknown", wantArgs: []string{"daemon", "-config", "config.yaml"}},
		{name: "removed daemon alias", input: []string{"d"}, wantCommand: "unknown", wantArgs: []string{"d"}},
		{name: "unknown command", input: []string{"unknown"}, wantCommand: "unknown", wantArgs: []string{"unknown"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command, args := routeCLI(tc.input)
			if command != tc.wantCommand || !slices.Equal(args, tc.wantArgs) {
				t.Fatalf("routeCLI(%v) = (%q, %v), want (%q, %v)", tc.input, command, args, tc.wantCommand, tc.wantArgs)
			}
		})
	}
}

func TestParseForegroundOptionsUsesExplicitConfigWithoutResolver(t *testing.T) {
	resolverCalled := false
	opts, err := parseForegroundOptions([]string{"-config", " /custom/config.yaml ", "-control-listen", " 127.0.0.1:9999 "}, func() (string, error) {
		resolverCalled = true
		return "", os.ErrNotExist
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalled {
		t.Fatal("default config resolver was called for an explicit -config")
	}
	if opts.configPath != "/custom/config.yaml" || opts.controlListen != "127.0.0.1:9999" {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestParseForegroundOptionsUsesDefaultAndRejectsPositionals(t *testing.T) {
	opts, err := parseForegroundOptions(nil, func() (string, error) {
		return "/opt/vmflow/config.yaml", nil
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.configPath != "/opt/vmflow/config.yaml" {
		t.Fatalf("default config = %q", opts.configPath)
	}
	if _, err := parseForegroundOptions([]string{"unexpected"}, func() (string, error) {
		return "/opt/vmflow/config.yaml", nil
	}, io.Discard); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("positional argument error = %v", err)
	}
	if _, err := parseForegroundOptions([]string{"-h"}, func() (string, error) {
		return "/opt/vmflow/config.yaml", nil
	}, io.Discard); err != flag.ErrHelp {
		t.Fatalf("help error = %v, want flag.ErrHelp", err)
	}
}

func TestConfigPathBesideExecutableResolvesSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real dir")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realBinary := filepath.Join(realDir, "vmflow")
	if err := os.WriteFile(realBinary, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "links")
	if err := os.Mkdir(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "vmflow")
	if err := os.Symlink(realBinary, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := configPathBesideExecutable(link)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realDir, "config.yaml")
	if got != want {
		t.Fatalf("config path = %q, want %q", got, want)
	}
	if _, err := configPathBesideExecutable(filepath.Join(dir, "missing-vmflow")); err == nil {
		t.Fatal("missing executable should fail closed")
	}
}

func TestRunForwardingReportsReadyAfterBindingControlListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	done := make(chan error, 1)
	cfg := config.File{ControlListenAddr: "127.0.0.1:0"}

	go func() {
		done <- runForwardingWithReady(ctx, cfg, cfg, "test-config.yaml", testLogger(), false, ready)
	}()

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("readiness failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for readiness")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestRunForwardingReportsListenFailureBeforeReady(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	ready := make(chan error, 1)
	done := make(chan error, 1)
	cfg := config.File{ControlListenAddr: occupied.Addr().String()}
	go func() {
		done <- runForwardingWithReady(context.Background(), cfg, cfg, "test-config.yaml", testLogger(), false, ready)
	}()

	select {
	case err := <-ready:
		if err == nil || !strings.Contains(err.Error(), "listen on control address") {
			t.Fatalf("readiness error = %v, want listen failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for readiness failure")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listen on control address") {
			t.Fatalf("run error = %v, want listen failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forwarding failure")
	}
}

func TestBotControlClientUsesAuthenticatedInProcessTransport(t *testing.T) {
	var calls int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.RemoteAddr != "127.0.0.1:0" {
			t.Errorf("RemoteAddr = %q", r.RemoteAddr)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/reload" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config_path":"config.yaml","rule_count":0}`))
	})
	client := newBotControlFn(handler, testLogger())("admin-secret")
	if client == nil {
		t.Fatal("control client is nil")
	}
	if client.BaseURL() != "http://vmflow.internal" {
		t.Fatalf("BaseURL = %q", client.BaseURL())
	}
	if _, err := client.Reload(context.Background()); err != nil {
		t.Fatalf("in-process reload: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
}

func TestBotControlClientWithoutTokenIsReadOnly(t *testing.T) {
	client := newBotControlFn(http.NotFoundHandler(), testLogger())(" \t ")
	if client != nil {
		t.Fatal("empty control token should not create a write client")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
