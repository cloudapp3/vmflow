//go:build linux || darwin

package uninstaller

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCertPathsFromConfig(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "control.crt")
	key := filepath.Join(dir, "control.key")
	ca := filepath.Join(dir, "clients-ca.crt")
	acme := filepath.Join(dir, "acme")
	cc := filepath.Join(dir, "certcache")

	cfg := strings.Join([]string{
		"version: 1",
		"control_tls:",
		"  cert_file: " + cert,
		"  key_file: " + key,
		"  client_ca_file: " + ca,
		"acme_cache_dir: " + acme,
		"cert_cache_dir: " + cc,
		"",
	}, "\n")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := certPathsFromConfig(cfgPath)
	if len(paths) != 5 {
		t.Fatalf("got %d paths %v, want 5", len(paths), paths)
	}
	want := map[string]bool{cert: true, key: true, ca: true, acme: true, cc: true}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

func TestCertPathsFromConfigUnparseable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// config.Load rejects unsupported versions, so certPathsFromConfig yields nil.
	if err := os.WriteFile(cfgPath, []byte("version: 999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := certPathsFromConfig(cfgPath); got != nil {
		t.Fatalf("expected no paths for unparseable config, got %v", got)
	}
}

func TestCertKind(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := certKind(file); got != kindFile {
		t.Errorf("certKind(file)=%q want %q", got, kindFile)
	}
	if got := certKind(sub); got != kindDir {
		t.Errorf("certKind(dir)=%q want %q", got, kindDir)
	}
	if got := certKind(filepath.Join(dir, "missing")); got != kindFile {
		t.Errorf("certKind(missing)=%q want %q", got, kindFile)
	}
}

func TestIsProtected(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root", "/", true},
		{"etc", "/etc", true},
		{"var", "/var", true},
		{"usr", "/usr", true},
		{"etc_vmflow", "/etc/vmflow", false},
		{"var_log_vmflow", "/var/log/vmflow", false},
		{"binary", "/usr/local/bin/vmflow", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isProtected(c.path); got != c.want {
				t.Errorf("isProtected(%q)=%v want %v", c.path, got, c.want)
			}
		})
	}
}

func TestExecuteRemovesFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested", "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []Item{
		{Kind: kindFile, Path: file},
		{Kind: kindDir, Path: sub},
	}
	if err := Execute(io.Discard, items); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pathExists(file) {
		t.Errorf("%s should have been removed", file)
	}
	if pathExists(sub) {
		t.Errorf("%s should have been removed", sub)
	}
}

func TestExecuteIsIdempotent(t *testing.T) {
	// Removing a non-existent path is not an error.
	items := []Item{
		{Kind: kindFile, Path: filepath.Join(t.TempDir(), "nope")},
		{Kind: kindDir, Path: filepath.Join(t.TempDir(), "nodir")},
	}
	if err := Execute(io.Discard, items); err != nil {
		t.Fatalf("Execute on absent paths: %v", err)
	}
}

func TestExecuteRefusesProtectedPath(t *testing.T) {
	items := []Item{{Kind: kindDir, Path: "/etc"}}
	err := Execute(io.Discard, items)
	if err == nil {
		t.Fatal("expected error for protected path, got nil")
	}
	if _, statErr := os.Stat("/etc"); statErr != nil {
		t.Errorf("/etc should still exist: %v", statErr)
	}
}

func TestRemoveSelf(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fakebin")
	if err := os.WriteFile(f, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeSelf(f, io.Discard); err != nil {
		t.Fatalf("removeSelf: %v", err)
	}
	if pathExists(f) {
		t.Error("file should have been removed")
	}
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"anything\n", false},
	}
	for _, c := range cases {
		got, err := Confirm(io.Discard, strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("Confirm(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Confirm(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestPackageOwnerUnmanaged(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notapkg")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if owner := packageOwner(f); owner != "" {
		t.Errorf("packageOwner(unmanaged)=%q want empty", owner)
	}
}

func TestPlanIncludesRunningBinary(t *testing.T) {
	items, _ := Plan()
	var bin *Item
	for i := range items {
		if items[i].Self {
			bin = &items[i]
			break
		}
	}
	if bin == nil {
		t.Fatal("Plan did not include the running binary item")
	}
	if bin.Kind != kindBinary || bin.Path == "" {
		t.Errorf("binary item malformed: %+v", bin)
	}
}
