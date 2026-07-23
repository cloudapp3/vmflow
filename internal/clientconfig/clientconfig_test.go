package clientconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client", "client.yaml")
	want := Profile{
		Address:    "http://127.0.0.1:19090/",
		Token:      "secret-token",
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != profileVersion || got.Address != "http://127.0.0.1:19090" || got.Token != want.Token || got.ConfigPath != want.ConfigPath {
		t.Fatalf("profile = %+v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("profile mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestLoadRejectsInsecureOrInvalidProfile(t *testing.T) {
	dir := t.TempDir()
	valid := "version: 1\naddress: http://127.0.0.1:19090\ntoken: secret\n"

	badAddress := filepath.Join(dir, "remote.yaml")
	if err := os.WriteFile(badAddress, []byte(strings.Replace(valid, "127.0.0.1", "example.com", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(badAddress); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("remote address error = %v", err)
	}

	if runtime.GOOS != "windows" {
		wide := filepath.Join(dir, "wide.yaml")
		if err := os.WriteFile(wide, []byte(valid), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(wide); err == nil || !strings.Contains(err.Error(), "permissions") {
			t.Fatalf("wide permission error = %v", err)
		}
	}

	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err == nil {
		if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink error = %v", err)
		}
	}
}

func TestDefaultPathOverrideMustBeAbsolute(t *testing.T) {
	t.Setenv("VMFLOW_CLIENT_CONFIG", "relative/client.yaml")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("relative override should fail")
	}
	path := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", path)
	if got, err := DefaultPath(); err != nil || got != path {
		t.Fatalf("DefaultPath = %q, %v", got, err)
	}
}
