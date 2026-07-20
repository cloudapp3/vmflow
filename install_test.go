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
	const reviewHint = "Review forwarding rules before starting or restarting vmflow"
	if !strings.Contains(out, reviewHint) || !strings.Contains(out, "enabled public listeners") {
		t.Fatalf("installer output does not prompt config review:\n%s", out)
	}

	custom := "version: 1\n# operator config\nrules: []\n"
	if err := os.WriteFile(configPath, []byte(custom), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o640); err != nil {
		t.Fatal(err)
	}
	upgradeOut := runInstallScript(t, fixture, installDir, true)
	assertFileContent(t, configPath, custom)
	assertFileMode(t, configPath, 0o640)
	if !strings.Contains(upgradeOut, "Preserved existing config") || !strings.Contains(upgradeOut, reviewHint) {
		t.Fatalf("upgrade output does not warn about preserved rules:\n%s", upgradeOut)
	}
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

func TestInstallScriptAcceptsLegacyArchiveConfigPath(t *testing.T) {
	fixture := newInstallFixture(t, "examples/config.yaml")
	installDir := t.TempDir()

	out := runInstallScriptArgs(t, fixture, true, "--dir", installDir)
	assertFileContent(t, filepath.Join(installDir, "vmflow"), "test-binary\n")
	assertFileContent(t, filepath.Join(installDir, "config.yaml"), "version: 1\nrules: []\n")
	assertFileContent(t, filepath.Join(installDir, testConfigMarker), "vmflow\n")
	if !strings.Contains(out, "Resolving latest release") ||
		!strings.Contains(out, "Installed vmflow v"+testInstallVersion) {
		t.Fatalf("default-version legacy install output is incomplete:\n%s", out)
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

func TestInstallScriptAutoUserInstallUpdatesZshPathOnce(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installFakeUID(t, fixture.fakeBin, "1000")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	installFakeSudo(t, fixture.fakeBin)
	t.Setenv("FAKE_SUDO_LOG", sudoLog)
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")

	args := []string{"--version", "v" + testInstallVersion}
	out := runInstallScriptArgs(t, fixture, true, args...)
	installDir := filepath.Join(home, ".local", "bin")
	assertFileContent(t, filepath.Join(installDir, "vmflow"), "test-binary\n")
	assertFileContent(t, filepath.Join(installDir, "config.yaml"), "version: 1\nrules: []\n")
	if !strings.Contains(out, "Added "+installDir+" to "+filepath.Join(home, ".zshrc")) {
		t.Fatalf("installer did not report the persistent PATH update:\n%s", out)
	}

	secondOut := runInstallScriptArgs(t, fixture, true, args...)
	rc, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	pathLine := `export PATH=` + installDir + `:$PATH`
	if count := strings.Count(string(rc), pathLine); count != 1 {
		t.Fatalf("PATH line count = %d, want 1:\n%s", count, rc)
	}
	if strings.Contains(secondOut, "Added "+installDir) ||
		!strings.Contains(secondOut, installDir+" is already configured in "+filepath.Join(home, ".zshrc")) {
		t.Fatalf("repeat install reported the wrong PATH state:\n%s", secondOut)
	}
	if _, err := os.Lstat(sudoLog); !os.IsNotExist(err) {
		t.Fatalf("automatic user install unexpectedly invoked sudo: %v", err)
	}
}

func TestInstallScriptDelegatedUninstallRemovesOnlyOwnedPathBlock(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := filepath.Join(t.TempDir(), "home with spaces")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")

	args := []string{"--version", "v" + testInstallVersion}
	runInstallScriptArgs(t, fixture, true, args...)
	installDir := filepath.Join(home, ".local", "bin")
	rcPath := filepath.Join(home, ".zshrc")
	quotedInstallDir := strings.ReplaceAll(installDir, " ", `\ `)
	pathLine := `export PATH=` + quotedInstallDir + `:$PATH`
	operatorBlock := "# vmflow user install\nexport PATH=/operator/bin:$PATH\n" + pathLine + "\n"
	otherInstallDir := filepath.Join(home, "bin")
	if err := os.Mkdir(otherInstallDir, 0o700); err != nil {
		t.Fatal(err)
	}
	otherBinary := filepath.Join(otherInstallDir, "vmflow")
	writeExecutable(t, otherBinary, "other-install\n")
	otherPathLine := `export PATH=` + strings.ReplaceAll(otherInstallDir, " ", `\ `) + `:$PATH`
	otherBlock := "# vmflow user install\n" + otherPathLine + "\n"
	rc, err := os.OpenFile(rcPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(rc, operatorBlock+otherBlock); err != nil {
		rc.Close()
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(installDir, "vmflow"), `#!/bin/sh
set -eu
if [ "${1:-}" = "uninstall" ] && [ "${2:-}" = "--help" ]; then
  exit 0
fi
if [ "${1:-}" = "uninstall" ]; then
  dir=${0%/*}
  /bin/rm -f -- "$dir/config.yaml" "$dir/.vmflow-config-owned" "$0"
  exit 0
fi
exit 1
`)
	out := runInstallScriptArgs(t, fixture, true, "--uninstall", "--dir", installDir)
	if !strings.Contains(out, "Removed vmflow PATH entry from "+rcPath) {
		t.Fatalf("uninstall did not report PATH cleanup:\n%s", out)
	}
	for _, name := range []string{"vmflow", "config.yaml", testConfigMarker} {
		if _, err := os.Lstat(filepath.Join(installDir, name)); !os.IsNotExist(err) {
			t.Fatalf("delegated uninstall left %s behind: %v", name, err)
		}
	}
	assertFileContent(t, otherBinary, "other-install\n")
	raw, err := os.ReadFile(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "# vmflow user install"); got != 2 {
		t.Fatalf("marker count = %d, want the operator and other-install blocks:\n%s", got, raw)
	}
	if got := strings.Count(string(raw), pathLine); got != 1 {
		t.Fatalf("matching PATH line count = %d, want standalone operator line preserved:\n%s", got, raw)
	}
	if !strings.Contains(string(raw), operatorBlock) {
		t.Fatalf("uninstall changed the operator PATH content:\n%s", raw)
	}
	if !strings.Contains(string(raw), otherBlock) {
		t.Fatalf("uninstall removed the other active installation's PATH block:\n%s", raw)
	}
}

func TestInstallScriptUninstallCleansStalePathBlockWithoutInstalledFiles(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installDir := filepath.Join(home, ".local", "bin")
	pathBlock := "before\n\n# vmflow user install\nexport PATH=" + installDir + ":$PATH\nafter\n"
	rcPath := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(rcPath, []byte(pathBlock), 0o640); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, "profile-victim")
	if err := os.WriteFile(victim, []byte(pathBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(home, ".profile")); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fixture.fakeBin, "uname"), "#!/bin/sh\nprintf 'TestOS\\n'\n")
	writeExecutable(t, filepath.Join(fixture.fakeBin, "systemctl"), "#!/bin/sh\nexit 0\n")
	t.Setenv("HOME", home)
	t.Setenv("PATH", fixture.fakeBin+string(os.PathListSeparator)+"/usr/bin:/bin")

	out := runInstallScriptArgs(t, fixture, true, "--uninstall", "--dir", installDir)
	if !strings.Contains(out, "stale shell PATH entries were removed") ||
		!strings.Contains(out, filepath.Join(home, ".profile")+" is not a regular file") {
		t.Fatalf("stale cleanup output is incomplete:\n%s", out)
	}
	assertFileContent(t, rcPath, "before\n\nafter\n")
	assertFileContent(t, victim, pathBlock)
}

func TestInstallScriptRejectsInvalidOptionValuesBeforeDownload(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		installDir string
		want       string
	}{
		{name: "version consumes option", args: []string{"--version", "--system"}, want: "invalid value for --version"},
		{name: "dir consumes option", args: []string{"--dir", "--system"}, want: "invalid value for --dir"},
		{name: "empty version equals", args: []string{"--version="}, want: "empty value for --version"},
		{name: "empty dir equals", args: []string{"--dir="}, want: "empty value for --dir"},
		{name: "option-like version equals", args: []string{"--version=--system"}, want: "invalid value for --version"},
		{name: "option-like dir equals", args: []string{"--dir=--system"}, want: "invalid value for --dir"},
		{
			name:       "option-like dir environment",
			args:       []string{"--version", "v" + testInstallVersion},
			installDir: "--target-directory=/tmp",
			want:       "invalid value for VMFLOW_INSTALL_DIR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newInstallFixture(t, "config.yaml")
			curlMarker := filepath.Join(t.TempDir(), "curl-called")
			t.Setenv("FAKE_CURL_MARKER", curlMarker)
			if tc.installDir != "" {
				t.Setenv("VMFLOW_INSTALL_DIR", tc.installDir)
			}
			writeExecutable(t, filepath.Join(fixture.fakeBin, "curl"), `#!/bin/sh
set -eu
: >"$FAKE_CURL_MARKER"
exit 99
`)

			out := runInstallScriptArgs(t, fixture, false, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("unexpected argument validation failure:\n%s", out)
			}
			if _, err := os.Lstat(curlMarker); !os.IsNotExist(err) {
				t.Fatalf("installer reached curl before rejecting arguments: %v", err)
			}
		})
	}
}

func TestInstallScriptDirArgumentOverridesInvalidEnvironment(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installDir := t.TempDir()
	t.Setenv("VMFLOW_INSTALL_DIR", "--target-directory=/tmp")

	runInstallScript(t, fixture, installDir, true)
	assertFileContent(t, filepath.Join(installDir, "vmflow"), "test-binary\n")
}

func TestInstallScriptReadmeExportMakesBinaryResolvable(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")

	const script = `
VMFLOW_BIN_DIR="$(
  bash install.sh --version "$1" --print-install-dir
)" \
  && [ -n "$VMFLOW_BIN_DIR" ] \
  && [ "${VMFLOW_BIN_DIR#/}" != "$VMFLOW_BIN_DIR" ] \
  && [ -x "$VMFLOW_BIN_DIR/vmflow" ] \
  && export PATH="$VMFLOW_BIN_DIR:$PATH" \
  && command -v vmflow
`
	cmd := exec.Command("bash", "-c", script, "vmflow-readme-test", "v"+testInstallVersion)
	cmd.Env = append(os.Environ(),
		"PATH="+fixture.fakeBin+string(os.PathListSeparator)+"/usr/bin:/bin",
		"FAKE_ARCHIVE="+fixture.archive,
		"FAKE_CHECKSUMS="+fixture.checksums,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("README PATH integration failed: %v\n%s", err, out)
	}
	want := filepath.Join(home, ".local", "bin", "vmflow")
	if !strings.Contains(string(out), want+"\n") {
		t.Fatalf("current shell did not resolve the installed binary %q:\n%s", want, out)
	}
}

func TestInstallScriptAutoUserInstallPrefersUserBinInPath(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	userBin := filepath.Join(home, "bin")
	if err := os.Mkdir(userBin, 0o700); err != nil {
		t.Fatal(err)
	}
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", userBin+string(os.PathListSeparator)+"/usr/bin:/bin")

	out := runInstallScriptArgs(t, fixture, true, "--version", "v"+testInstallVersion)
	assertFileContent(t, filepath.Join(userBin, "vmflow"), "test-binary\n")
	if !strings.Contains(out, "Verify the installation with:\n  vmflow version") {
		t.Fatalf("installer did not report an immediately runnable command:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("installer unexpectedly modified shell startup file: %v", err)
	}
}

func TestInstallScriptExplicitDirDoesNotModifyShellPath(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")

	installDir := filepath.Join(home, "custom", "bin")
	runInstallScript(t, fixture, installDir, true)
	if _, err := os.Lstat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("explicit --dir unexpectedly modified shell startup file: %v", err)
	}
}

func TestInstallScriptNoModifyPathLeavesShellFilesUntouched(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")

	out := runInstallScriptArgs(t, fixture, true,
		"--version", "v"+testInstallVersion,
		"--no-modify-path",
	)
	if _, err := os.Lstat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("--no-modify-path unexpectedly modified shell startup file: %v", err)
	}
	if !strings.Contains(out, "is not in your PATH") {
		t.Fatalf("installer did not retain the PATH warning:\n%s", out)
	}
}

func TestInstallScriptSystemModeUsesSudoForTargetWrites(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installFakeUID(t, fixture.fakeBin, "1000")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	sudoPath := installFakeSudo(t, fixture.fakeBin)
	t.Setenv("FAKE_SUDO_LOG", sudoLog)
	t.Setenv("PATH", "/usr/bin:/bin")

	installDir := filepath.Join(t.TempDir(), "system", "bin")
	out := runInstallScriptArgs(t, fixture, true,
		"--version", "v"+testInstallVersion,
		"--system",
		"--dir", installDir,
	)
	assertFileContent(t, filepath.Join(installDir, "vmflow"), "test-binary\n")
	assertFileContent(t, filepath.Join(installDir, "config.yaml"), "version: 1\nrules: []\n")
	assertFileContent(t, filepath.Join(installDir, testConfigMarker), "vmflow\n")

	raw, err := os.ReadFile(sudoLog)
	if err != nil {
		t.Fatal(err)
	}
	logOutput := string(raw)
	for _, want := range []string{"-n -v", "mkdir -p " + installDir, "install -m 0755", "sh -c"} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("sudo log does not contain %q:\n%s", want, logOutput)
		}
	}

	targetPath := filepath.Join(installDir, "vmflow")
	privilegedCommand := `"` + sudoPath + `" "` + targetPath + `"`
	if !strings.Contains(out, "Verify the root-owned system installation with:\n  "+privilegedCommand+" version") ||
		!strings.Contains(out, "Start vmflow as root") || !strings.Contains(out, "\n  "+privilegedCommand+"\n") {
		t.Fatalf("system install did not report root startup commands:\n%s", out)
	}
}

func TestInstallScriptSystemModeRequiresSudoBeforeDownload(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("VMFLOW_SUDO", filepath.Join(t.TempDir(), "missing-sudo"))
	curlMarker := filepath.Join(t.TempDir(), "curl-called")
	t.Setenv("FAKE_CURL_MARKER", curlMarker)
	writeExecutable(t, filepath.Join(fixture.fakeBin, "curl"), `#!/bin/sh
set -eu
: >"$FAKE_CURL_MARKER"
exit 99
`)

	out := runInstallScriptArgs(t, fixture, false,
		"--version", "v"+testInstallVersion,
		"--system",
	)
	if !strings.Contains(out, "configured sudo command is not executable") {
		t.Fatalf("unexpected missing-sudo failure:\n%s", out)
	}
	if _, err := os.Lstat(curlMarker); !os.IsNotExist(err) {
		t.Fatalf("installer reached the download before rejecting sudo: %v", err)
	}
}

func TestInstallScriptRootSystemModeDoesNotUseSudo(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installFakeUID(t, fixture.fakeBin, "0")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("VMFLOW_SUDO", filepath.Join(t.TempDir(), "missing-sudo"))
	installDir := filepath.Join(t.TempDir(), "system", "bin")

	out := runInstallScriptArgs(t, fixture, true,
		"--version", "v"+testInstallVersion,
		"--system",
		"--dir", installDir,
	)
	targetPath := filepath.Join(installDir, "vmflow")
	assertFileContent(t, targetPath, "test-binary\n")
	if strings.Contains(out, "root-owned system installation") ||
		!strings.Contains(out, "Start vmflow (loads ") || !strings.Contains(out, "\n  \""+targetPath+"\"\n") {
		t.Fatalf("root system install did not report a direct startup command:\n%s", out)
	}
}

func TestInstallScriptRootUpgradeReusesExistingUserInstall(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(installDir, "vmflow"), "old-binary\n")
	const existingConfig = "version: 1\n# existing root rules\nrules: []\n"
	if err := os.WriteFile(filepath.Join(installDir, "config.yaml"), []byte(existingConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, testConfigMarker), []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installFakeUID(t, fixture.fakeBin, "0")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")

	out := runInstallScriptArgs(t, fixture, true, "--version", "v"+testInstallVersion)
	assertFileContent(t, filepath.Join(installDir, "vmflow"), "test-binary\n")
	assertFileContent(t, filepath.Join(installDir, "config.yaml"), existingConfig)
	if !strings.Contains(out, "Using existing installation directory: "+installDir) {
		t.Fatalf("installer did not reuse the existing root install:\n%s", out)
	}
	assertFileContentContains(t, filepath.Join(home, ".zshrc"), "export PATH="+installDir+":$PATH")
}

func TestInstallScriptReusesActiveConventionalInstallBeforeStaleCopy(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	staleDir := filepath.Join(home, ".local", "bin")
	activeDir := filepath.Join(home, "bin")
	for _, dir := range []string{staleDir, activeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("version: 1\nrules: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(staleDir, "vmflow"), "stale-binary\n")
	writeExecutable(t, filepath.Join(activeDir, "vmflow"), "active-binary\n")
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", activeDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	out := runInstallScriptArgs(t, fixture, true, "--version", "v"+testInstallVersion)
	assertFileContent(t, filepath.Join(activeDir, "vmflow"), "test-binary\n")
	assertFileContent(t, filepath.Join(staleDir, "vmflow"), "stale-binary\n")
	if !strings.Contains(out, "Using existing installation directory: "+activeDir) {
		t.Fatalf("installer did not reuse the active install:\n%s", out)
	}
}

func TestInstallScriptUnsupportedShellDoesNotWriteProfile(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	home := t.TempDir()
	installFakeUID(t, fixture.fakeBin, "1000")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/usr/bin/fish")
	t.Setenv("PATH", "/usr/bin:/bin")

	out := runInstallScriptArgs(t, fixture, true, "--version", "v"+testInstallVersion)
	if _, err := os.Lstat(filepath.Join(home, ".profile")); !os.IsNotExist(err) {
		t.Fatalf("unsupported shell unexpectedly modified .profile: %v", err)
	}
	if !strings.Contains(out, "unsupported shell fish") {
		t.Fatalf("installer did not warn about the unsupported shell:\n%s", out)
	}
}

func TestInstallScriptPrintInstallDirUsesStdoutOnly(t *testing.T) {
	fixture := newInstallFixture(t, "config.yaml")
	installDir := filepath.Join(t.TempDir(), "install dir")
	cmd := installScriptCommand(t, fixture,
		"--version", "v"+testInstallVersion,
		"--dir", installDir,
		"--print-install-dir",
	)
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("installer failed: %v", err)
	}
	if got, want := string(stdout), installDir+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
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
	*/releases/latest)
		printf '  "tag_name": "%s",\n' "$FAKE_VERSION"
		exit 0
		;;
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
	return runInstallScriptArgs(t, fixture, wantSuccess,
		"--version", "v"+testInstallVersion,
		"--dir", installDir,
	)
}

func runInstallScriptArgs(t *testing.T, fixture installFixture, wantSuccess bool, args ...string) string {
	t.Helper()
	cmd := installScriptCommand(t, fixture, args...)
	out, err := cmd.CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !wantSuccess && err == nil {
		t.Fatalf("installer unexpectedly succeeded:\n%s", out)
	}
	return string(out)
}

func installScriptCommand(t *testing.T, fixture installFixture, args ...string) *exec.Cmd {
	t.Helper()
	cmdArgs := append([]string{"install.sh"}, args...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Env = append(os.Environ(),
		"PATH="+fixture.fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_ARCHIVE="+fixture.archive,
		"FAKE_CHECKSUMS="+fixture.checksums,
		"FAKE_VERSION=v"+testInstallVersion,
	)
	return cmd
}

func installFakeUID(t *testing.T, fakeBin, uid string) {
	t.Helper()
	t.Setenv("FAKE_UID", uid)
	writeExecutable(t, filepath.Join(fakeBin, "id"), `#!/bin/sh
set -eu
if [ "${1:-}" = "-u" ]; then
  printf '%s\n' "$FAKE_UID"
  exit 0
fi
exec /usr/bin/id "$@"
`)
}

func installFakeSudo(t *testing.T, fakeBin string) string {
	t.Helper()
	sudoPath := filepath.Join(fakeBin, "sudo")
	t.Setenv("VMFLOW_SUDO", sudoPath)
	writeExecutable(t, sudoPath, `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_SUDO_LOG"
if [ "${1:-}" = "-n" ] && [ "${2:-}" = "-v" ]; then
  exit 0
fi
if [ "${1:-}" = "-v" ]; then
  exit 0
fi
exec "$@"
`)
	return sudoPath
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
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

func assertFileContentContains(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), want) {
		t.Fatalf("%s does not contain %q: %q", path, want, raw)
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
