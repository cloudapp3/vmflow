package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
)

func TestExecuteInitCreatesRuleAuthAndClientProfile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	profilePath := filepath.Join(dir, "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", profilePath)
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")
	port := availableTCPPort(t)

	var output bytes.Buffer
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: port,
		targetAddr: "127.0.0.1", targetPort: 22, name: "Local SSH", yes: true,
	}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("executeInit: %v\n%s", err, output.String())
	}
	if result.Start || result.Rule.RuleID != "local-ssh" || result.ProfilePath != profilePath {
		t.Fatalf("result = %+v", result)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Enabled || len(cfg.Auth.Tokens) != 1 || len(cfg.Rules) != 1 || !cfg.Rules[0].Enabled {
		t.Fatalf("config = %+v", cfg)
	}
	profile, err := clientconfig.Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Token != cfg.Auth.Tokens[0].Token || profile.ConfigPath != configPath {
		t.Fatalf("profile/config mismatch: %+v / %+v", profile, cfg.Auth)
	}
	if strings.Contains(output.String(), profile.Token) {
		t.Fatal("management token leaked to output")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %v, err = %v", info.Mode(), err)
		}
	}
}

func TestExecuteInitReplacesUntouchedBundledExample(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	port := availableTCPPort(t)
	_, err = executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: port,
		targetAddr: "127.0.0.1", targetPort: 22, name: "replacement", yes: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].RuleID != "replacement" {
		t.Fatalf("bundled example was not replaced: %+v", cfg.Rules)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "bundled example is loopback-only and disabled") || !strings.Contains(string(saved), "Forwarding rules created by vmflow init") {
		t.Fatalf("bundled example comment was not replaced:\n%s", saved)
	}
}

func TestExecuteInitSecuresWorldReadableConfigBeforeAddingToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nauth:\n  enabled: false\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	_, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "secure", yes: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSecureConfigPermissionsPreservesGroupReadableConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nrules: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := secureConfigPermissions(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("0640 service config should not be changed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %o, want 640", info.Mode().Perm())
	}
}

func TestSecureConfigPermissionsRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink semantics")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := secureConfigPermissions(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode = %o, want 644", info.Mode().Perm())
	}
}

func TestExecuteInitRequiresExposureConfirmation(t *testing.T) {
	dir := t.TempDir()
	options := initOptions{
		configPath: filepath.Join(dir, "config.yaml"), protocol: "tcp", listenAddr: "0.0.0.0", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "public",
	}
	_, err := executeInit(options, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "remote traffic") {
		t.Fatalf("exposure error = %v", err)
	}
	if _, statErr := os.Stat(options.configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config should not be written: %v", statErr)
	}
}

func TestExecuteInitNoAuthLeavesTUIReadOnly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	profilePath := filepath.Join(dir, "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", profilePath)
	_, err := executeInit(initOptions{
		configPath: configPath, protocol: "udp", listenAddr: "127.0.0.1", listenPort: availableUDPPort(t),
		targetAddr: "1.1.1.1", targetPort: 53, name: "dns", yes: true, noAuth: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Enabled {
		t.Fatal("auth unexpectedly enabled")
	}
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("client profile unexpectedly written: %v", err)
	}
}

func TestRuleIDAndPlaceholderHelpers(t *testing.T) {
	if got := slugRuleID("  SSH / Tokyo 01 "); got != "ssh-tokyo-01" {
		t.Fatalf("slugRuleID = %q", got)
	}
	if !isPlaceholderToken("change-me") || isPlaceholderToken("real-secret-value") {
		t.Fatal("placeholder classification is wrong")
	}
}

func TestReplaceClientProfileRestoresPreviousState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	oldProfile := clientconfig.Profile{Address: "http://127.0.0.1:19090", Token: "old-token"}
	if err := clientconfig.Save(path, oldProfile); err != nil {
		t.Fatal(err)
	}
	restore, err := replaceClientProfile(path, clientconfig.Profile{Address: "http://127.0.0.1:19123", Token: "new-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	got, err := clientconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != oldProfile.Address || got.Token != oldProfile.Token {
		t.Fatalf("restored profile = %+v", got)
	}

	newPath := filepath.Join(t.TempDir(), "new-client.yaml")
	restore, err = replaceClientProfile(newPath, clientconfig.Profile{Address: "http://127.0.0.1:19090", Token: "temporary"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("new profile was not removed: %v", err)
	}
}

func availableTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func availableUDPPort(t *testing.T) int {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	port := connection.LocalAddr().(*net.UDPAddr).Port
	_ = connection.Close()
	return port
}
