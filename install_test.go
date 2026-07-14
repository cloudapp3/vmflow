//go:build linux || darwin

package vmflow

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testInstallVersion = "9.9.9"
	testConfigMarker   = ".vmflow-config-owned"
)

func TestInstallScriptCreatesAndPreservesColocatedConfig(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installDir := filepath.Join(t.TempDir(), "install dir")

	out := runInstallScript(t, fixture, installDir, true)
	configPath := filepath.Join(installDir, "config.yaml")
	markerPath := filepath.Join(installDir, testConfigMarker)
	assertFileContent(t, filepath.Join(installDir, "vmflow"), "test-binary\n")
	assertFileContent(t, configPath, "version: 1\nrules: []\n")
	assertFileMode(t, configPath, 0o600)
	assertFileContent(t, markerPath, "vmflow\n")
	assertFileMode(t, markerPath, 0o600)
	if !strings.Contains(out, `"`+filepath.Join(installDir, "vmflow")+`"`) {
		t.Fatalf("installer output does not show the direct runtime command:\n%s", out)
	}
	if strings.Contains(out, " daemon ") || strings.Contains(out, "daemon -config") {
		t.Fatalf("installer output still exposes removed daemon command:\n%s", out)
	}

	custom := "version: 1\n# operator config\nrules: []\n"
	if err := os.WriteFile(configPath, []byte(custom), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o640); err != nil {
		t.Fatal(err)
	}
	runInstallScript(t, fixture, installDir, true)
	assertFileContent(t, configPath, custom)
	assertFileMode(t, configPath, 0o640)
}

func TestInstallScriptDoesNotClaimPreexistingConfig(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installDir := t.TempDir()
	configPath := filepath.Join(installDir, "config.yaml")
	const existing = "operator-owned\n"
	if err := os.WriteFile(configPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	runInstallScript(t, fixture, installDir, true)
	assertFileContent(t, configPath, existing)
	assertFileMode(t, configPath, 0o644)
	if _, err := os.Lstat(filepath.Join(installDir, testConfigMarker)); !os.IsNotExist(err) {
		t.Fatalf("preexisting config unexpectedly received an ownership marker: %v", err)
	}
}

func TestInstallScriptRejectsLegacyArchiveConfigPath(t *testing.T) {
	fixture := newInstallFixture(t, "examples/config.yaml")
	installDir := t.TempDir()
	out := runInstallScript(t, fixture, installDir, false)
	if !strings.Contains(out, "is incompatible with this installer") || !strings.Contains(out, "top-level regular config.yaml") {
		t.Fatalf("unexpected legacy archive failure:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(installDir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("legacy archive unexpectedly installed config: %v", err)
	}
}

func TestInstallScriptRejectsExistingConfigSymlink(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installDir := t.TempDir()
	configPath := filepath.Join(installDir, "config.yaml")
	if err := os.Symlink(filepath.Join(installDir, "missing"), configPath); err != nil {
		t.Fatal(err)
	}

	out := runInstallScript(t, fixture, installDir, false)
	if !strings.Contains(out, "existing config path is not a regular file") {
		t.Fatalf("unexpected installer failure:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "vmflow")); !os.IsNotExist(err) {
		t.Fatalf("binary changed despite invalid config path: %v", err)
	}
}

func TestInstallScriptDoesNotFollowConfigRaceSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		targetName  string
		wantFailure string
	}{
		{
			name:        "config",
			targetName:  "config.yaml",
			wantFailure: "failed to create config.yaml without overwriting an existing path",
		},
		{
			name:        "ownership marker",
			targetName:  testConfigMarker,
			wantFailure: "ownership marker could not be created safely",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newInstallFixture(t, "config.yaml")
			installDir := t.TempDir()
			victim := filepath.Join(t.TempDir(), "victim")
			const victimContent = "must-not-change\n"
			if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
				t.Fatal(err)
			}

			// The installer validates config paths before installing the binary.
			// This shim inserts a symlink during that gap to exercise the exclusive
			// creation path deterministically.
			fakeInstall := `#!/bin/sh
set -eu
cp "$3" "$4"
chmod "$2" "$4"
ln -s "$FAKE_INSTALL_RACE_VICTIM" "$FAKE_INSTALL_RACE_TARGET"
`
			if err := os.WriteFile(filepath.Join(fixture.fakeBin, "install"), []byte(fakeInstall), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_INSTALL_RACE_VICTIM", victim)
			t.Setenv("FAKE_INSTALL_RACE_TARGET", filepath.Join(installDir, tc.targetName))

			out := runInstallScript(t, fixture, installDir, false)
			if !strings.Contains(out, tc.wantFailure) {
				t.Fatalf("unexpected installer failure:\n%s", out)
			}
			assertFileContent(t, victim, victimContent)
		})
	}
}

type installFixture struct {
	archive   string
	checksums string
	fakeBin   string
}

func newInstallFixture(t *testing.T, configEntry string) installFixture {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("installer does not support test architecture %s", runtime.GOARCH)
	}
	dir := t.TempDir()
	archiveName := fmt.Sprintf("vmflow-%s-%s-%s.tar.gz", testInstallVersion, runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(dir, archiveName)
	writeInstallArchive(t, archivePath, configEntry)
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	checksumsPath := filepath.Join(dir, "checksums.txt")
	sum := sha256.Sum256(raw)
	if err := os.WriteFile(checksumsPath, []byte(fmt.Sprintf("%x  %s\n", sum, archiveName)), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCurl := `#!/bin/sh
set -eu
out=""
url=""
need_out=0
for arg in "$@"; do
  if [ "$need_out" -eq 1 ]; then
    out="$arg"
    need_out=0
    continue
  fi
  case "$arg" in
    -o) need_out=1 ;;
    http://*|https://*) url="$arg" ;;
  esac
done
case "$url" in
  */checksums.txt) source="$FAKE_CHECKSUMS" ;;
  *.tar.gz) source="$FAKE_ARCHIVE" ;;
  *) echo "unexpected URL: $url" >&2; exit 1 ;;
esac
if [ -n "$out" ]; then
  cp "$source" "$out"
else
  cat "$source"
fi
`
	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	return installFixture{archive: archivePath, checksums: checksumsPath, fakeBin: fakeBin}
}

func writeInstallArchive(t *testing.T, path, configEntry string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name string
		mode int64
		data string
	}{
		{name: "vmflow", mode: 0o755, data: "test-binary\n"},
		{name: configEntry, mode: 0o644, data: "version: 1\nrules: []\n"},
	}
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func runInstallScript(t *testing.T, fixture installFixture, installDir string, wantSuccess bool) string {
	t.Helper()
	cmd := exec.Command("bash", "install.sh", "--version", "v"+testInstallVersion, "--dir", installDir)
	cmd.Env = append(os.Environ(),
		"PATH="+fixture.fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_ARCHIVE="+fixture.archive,
		"FAKE_CHECKSUMS="+fixture.checksums,
	)
	out, err := cmd.CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !wantSuccess && err == nil {
		t.Fatalf("installer unexpectedly succeeded:\n%s", out)
	}
	return string(out)
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("%s content = %q, want %q", path, raw, want)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
