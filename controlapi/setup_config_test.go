package controlapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
)

func TestSaveSetupConfigCreatesSecureValidatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	update := SetupConfigUpdate{
		Auth: config.AuthConfig{Enabled: true, Tokens: []config.AuthToken{{Name: "local-admin", Token: "secret", Role: config.AuthRoleAdmin}}},
		Rules: []engine.Rule{{
			RuleID: "ssh", Name: "ssh", Protocol: engine.ProtocolTCP,
			ListenAddr: "127.0.0.1", ListenPort: 2201,
			TargetAddr: "127.0.0.1", TargetPort: 22, Enabled: true,
		}},
	}
	got, err := SaveSetupConfig(path, update)
	if err != nil {
		t.Fatalf("SaveSetupConfig: %v", err)
	}
	if !got.Auth.Enabled || len(got.Rules) != 1 || !got.Rules[0].Enabled {
		t.Fatalf("saved config = %+v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestSaveSetupConfigPreservesUnknownFieldsCommentsAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("version: 1\ncustom_field: keep-me # custom\nauth:\n  enabled: false\nrules: [] # rules-comment\n")
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	update := SetupConfigUpdate{
		Auth: config.AuthConfig{Enabled: true, Tokens: []config.AuthToken{{Name: "admin", Token: "secret", Role: config.AuthRoleAdmin}}},
		Rules: []engine.Rule{{
			RuleID: "dns", Name: "dns", Protocol: engine.ProtocolUDP,
			ListenAddr: "127.0.0.1", ListenPort: 5353,
			TargetAddr: "1.1.1.1", TargetPort: 53, Enabled: true,
		}},
	}
	if _, err := SaveSetupConfig(path, update); err != nil {
		t.Fatalf("SaveSetupConfig: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if !strings.Contains(text, "custom_field: keep-me # custom") || !strings.Contains(text, "# rules-comment") {
		t.Fatalf("unknown field or comments lost:\n%s", text)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("config mode = %o, want 640", info.Mode().Perm())
		}
	}
}

func TestSaveSetupConfigRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := SaveSetupConfig(link, SetupConfigUpdate{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}
