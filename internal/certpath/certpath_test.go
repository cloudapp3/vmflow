package certpath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		allowWildcard bool
		want          string
		wantError     bool
	}{
		{name: "lowercase", input: " Example.COM. ", want: "example.com"},
		{name: "wildcard", input: "*.Example.COM", allowWildcard: true, want: "*.example.com"},
		{name: "ipv4", input: "192.0.2.1", want: "192.0.2.1"},
		{name: "ipv6", input: "2001:0db8::1", want: "2001:db8::1"},
		{name: "path traversal", input: "../../outside", wantError: true},
		{name: "windows traversal", input: `..\..\outside`, wantError: true},
		{name: "absolute path", input: "/tmp/outside", wantError: true},
		{name: "windows absolute path", input: `C:\outside`, wantError: true},
		{name: "encoded separator", input: "..%2foutside", wantError: true},
		{name: "empty label", input: "example..com", wantError: true},
		{name: "leading hyphen", input: "-example.com", wantError: true},
		{name: "trailing hyphen", input: "example-.com", wantError: true},
		{name: "embedded wildcard", input: "foo.*.example.com", allowWildcard: true, wantError: true},
		{name: "wildcard disallowed", input: "*.example.com", wantError: true},
		{name: "unicode requires alabel", input: "example.\u6d4b\u8bd5", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Normalize(test.input, test.allowWildcard)
			if test.wantError {
				if err == nil {
					t.Fatalf("Normalize(%q) succeeded with %q", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("Normalize(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeAllDeduplicates(t *testing.T) {
	got, err := NormalizeAll([]string{"Example.com", "example.com.", "api.example.com"}, false)
	if err != nil {
		t.Fatalf("NormalizeAll: %v", err)
	}
	want := []string{"example.com", "api.example.com"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeAll returned %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("NormalizeAll returned %v, want %v", got, want)
		}
	}
}

func TestPairRoundTripUsesHashedNames(t *testing.T) {
	root := t.TempDir()
	certPEM := []byte("certificate-data")
	keyPEM := []byte("private-key-data")
	if err := WritePair(root, Manual, "Example.COM.", certPEM, keyPEM); err != nil {
		t.Fatalf("WritePair: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "manual"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d stored files, want 2", len(entries))
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "example.com") || !strings.HasPrefix(entry.Name(), "d-") {
			t.Fatalf("unexpected certificate filename %q", entry.Name())
		}
	}

	gotCert, gotKey, err := ReadPair(root, Manual, "example.com")
	if err != nil {
		t.Fatalf("ReadPair: %v", err)
	}
	if string(gotCert) != string(certPEM) || string(gotKey) != string(keyPEM) {
		t.Fatalf("ReadPair returned %q/%q", gotCert, gotKey)
	}

	if runtime.GOOS != "windows" {
		paths, _, err := pathsFor(root, Manual, "example.com")
		if err != nil {
			t.Fatalf("pathsFor: %v", err)
		}
		keyInfo, err := os.Stat(paths.key)
		if err != nil {
			t.Fatalf("stat key: %v", err)
		}
		if keyInfo.Mode().Perm() != 0o600 {
			t.Fatalf("key mode = %o, want 600", keyInfo.Mode().Perm())
		}
	}

	if err := RemovePair(root, Manual, "example.com"); err != nil {
		t.Fatalf("RemovePair: %v", err)
	}
	if _, _, err := ReadPair(root, Manual, "example.com"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadPair after removal error = %v, want not exist", err)
	}
}

func TestReadPairMigratesLegacyFiles(t *testing.T) {
	root := t.TempDir()
	manualDir := filepath.Join(root, "manual")
	if err := os.MkdirAll(manualDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacyCert := filepath.Join(manualDir, "legacy.example.com.crt")
	legacyKey := filepath.Join(manualDir, "legacy.example.com.key")
	if err := os.WriteFile(legacyCert, []byte("legacy-cert"), 0o644); err != nil {
		t.Fatalf("write legacy cert: %v", err)
	}
	if err := os.WriteFile(legacyKey, []byte("legacy-key"), 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}

	certPEM, keyPEM, err := ReadPair(root, Manual, "legacy.example.com")
	if err != nil {
		t.Fatalf("ReadPair: %v", err)
	}
	if string(certPEM) != "legacy-cert" || string(keyPEM) != "legacy-key" {
		t.Fatalf("migrated pair = %q/%q", certPEM, keyPEM)
	}
	if _, err := os.Lstat(legacyCert); !os.IsNotExist(err) {
		t.Fatalf("legacy cert still exists: %v", err)
	}
	if _, err := os.Lstat(legacyKey); !os.IsNotExist(err) {
		t.Fatalf("legacy key still exists: %v", err)
	}
}

func TestTraversalIdentifiersCannotTouchOutsideFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside-sentinel")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, domain := range []string{"../../outside-sentinel", `..\..\outside-sentinel`, "/tmp/outside-sentinel", `C:\outside-sentinel`} {
		if err := WritePair(root, Manual, domain, []byte("changed"), []byte("changed")); err == nil {
			t.Errorf("WritePair(%q) unexpectedly succeeded", domain)
		}
		if err := RemovePair(root, Manual, domain); err == nil {
			t.Errorf("RemovePair(%q) unexpectedly succeeded", domain)
		}
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("outside sentinel changed to %q", data)
	}
}

func TestPairRejectsSymlinkTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges on Windows")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	paths, _, err := pathsFor(root, Manual, "example.com")
	if err != nil {
		t.Fatalf("pathsFor: %v", err)
	}
	if err := os.MkdirAll(paths.directory, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(outside, paths.cert); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := WritePair(root, Manual, "example.com", []byte("changed"), []byte("key")); err == nil {
		t.Fatal("WritePair unexpectedly followed a symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("outside file changed to %q", data)
	}
}

func TestMetadataRoundTripRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := WriteMetadata(root, []byte(`[{"domain":"example.com"}]`)); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	data, err := ReadMetadata(root)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !strings.Contains(string(data), "example.com") {
		t.Fatalf("metadata = %q", data)
	}
	if runtime.GOOS == "windows" {
		return
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	metadataPath := filepath.Join(root, metadataFilename)
	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}
	if err := os.Symlink(outside, metadataPath); err != nil {
		t.Fatalf("symlink metadata: %v", err)
	}
	if err := WriteMetadata(root, []byte("changed")); err == nil {
		t.Fatal("WriteMetadata unexpectedly followed a symlink")
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside: %v", err)
	}
	if string(outsideData) != "unchanged" {
		t.Fatalf("outside file changed to %q", outsideData)
	}
}
