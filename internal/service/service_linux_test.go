//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUnitContainsRequiredDirectives(t *testing.T) {
	cfg := Config{
		BinaryPath:  "/usr/local/bin/vmflow",
		ConfigPath:  "/etc/vmflow/config.yaml",
		ServiceName: "vmflow",
	}
	unit := systemdUnit(cfg)
	for _, want := range []string{
		"[Unit]",
		"Description=vmflow L4 forwarding daemon",
		"After=network-online.target",
		"Type=simple",
		"ExecStart=",
		"Restart=on-failure",
		"RestartSec=5",
		"NoNewPrivileges=true",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"StateDirectory=vmflow",
		"StateDirectoryMode=0750",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit missing %q\n---\n%s", want, unit)
		}
	}
	// ExecStart uses the explicit runtime command and keeps the config path quoted.
	if !strings.Contains(unit, `ExecStart="/usr/local/bin/vmflow" "run" "-config"`) {
		t.Errorf("ExecStart does not use the explicit runtime entry:\n%s", unit)
	}
	if strings.Contains(unit, `"daemon"`) {
		t.Errorf("ExecStart still contains removed daemon command:\n%s", unit)
	}
	if !strings.Contains(unit, `"/etc/vmflow/config.yaml"`) {
		t.Errorf("config path should be quoted in ExecStart:\n%s", unit)
	}
}

// TestSystemdUnitUserLine documents that --user opts into a dedicated account;
// the default unit runs as root (simplest for forwarding privileged ports).
func TestSystemdUnitUserLine(t *testing.T) {
	with := systemdUnit(Config{BinaryPath: "/x/vmflow", ConfigPath: "/c.yaml", ServiceName: "vmflow", User: "vmflow"})
	if !strings.Contains(with, "User=vmflow") || !strings.Contains(with, "Group=vmflow") {
		t.Errorf("expected User/Group lines when User set\n%s", with)
	}
	without := systemdUnit(Config{BinaryPath: "/x/vmflow", ConfigPath: "/c.yaml", ServiceName: "vmflow"})
	if strings.Contains(without, "User=") {
		t.Errorf("did not expect User= when User empty\n%s", without)
	}
}

func TestSystemdExecStartQuotesAndFlags(t *testing.T) {
	got := systemdExecStart(Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", LogFile: "/var/log/vmflow/v.log", ControlPort: 19123})
	if !strings.Contains(got, `"/usr/local/bin/vmflow"`) {
		t.Errorf("binary path should be quoted: %s", got)
	}
	if !strings.Contains(got, "-log-file") {
		t.Errorf("expected -log-file when LogFile set: %s", got)
	}
	if !strings.Contains(got, "19123") {
		t.Errorf("expected control port override: %s", got)
	}
	if !strings.Contains(got, `"-control-port" "19123"`) {
		t.Errorf("extra arguments must remain separate tokens: %s", got)
	}
}

// TestNormalizeResolvesDefaults mirrors the updater.New defaulting pattern.
func TestNormalizeResolvesDefaults(t *testing.T) {
	cfg := normalize(Config{})
	if cfg.ServiceName != "vmflow" {
		t.Errorf("ServiceName default = %q, want vmflow", cfg.ServiceName)
	}
	if cfg.ConfigPath != linuxDefaultCfg {
		t.Errorf("ConfigPath default = %q, want %q", cfg.ConfigPath, linuxDefaultCfg)
	}
	if cfg.BinaryPath == "" {
		t.Errorf("BinaryPath should resolve to current executable")
	}
}

// TestTrustedServiceBinaryPathRejectsBadInputs covers the deterministic
// pre-conditions before the platform trust walk runs.
func TestTrustedServiceBinaryPathRejectsBadInputs(t *testing.T) {
	for _, in := range []string{"", "  ", "relative/vmflow", "./vmflow"} {
		if _, err := trustedServiceBinaryPath(in); err == nil {
			t.Errorf("trustedServiceBinaryPath(%q) should error", in)
		}
	}
	if _, err := trustedServiceBinaryPath("/definitely/not/installed/vmflow"); err == nil {
		t.Fatalf("expected not-found error for missing binary")
	}
}

func TestTrustedServiceConfigPathRejectsBadInputs(t *testing.T) {
	for _, in := range []string{"", "  ", "relative/config.yaml", "./config.yaml"} {
		if _, err := trustedServiceConfigPath(in); err == nil {
			t.Errorf("trustedServiceConfigPath(%q) should error", in)
		}
	}
	if _, err := trustedServiceConfigPath("/definitely/not/installed/config.yaml"); err == nil {
		t.Fatal("expected not-found error for missing config")
	}
}

func TestTrustedServiceConfigRejectsUserWritableTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Skipf("cannot chmod temp dir: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedServiceConfigPath(configPath); err == nil {
		t.Fatal("expected rejection for config in a group-writable tree")
	}
}

func TestValidateServiceConfigParsesContents(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "valid.yaml")
	if err := os.WriteFile(valid, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateServiceConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("version: 99\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateServiceConfig(invalid); err == nil || !strings.Contains(err.Error(), "unsupported config version") {
		t.Fatalf("expected semantic config error, got %v", err)
	}
}

func TestInstallSystemdUnitRestartsAndVerifiesActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.service")
	if err := os.WriteFile(path, []byte("old unit\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var commands []string
	runner := func(argv []string) ([]byte, error) {
		commands = append(commands, strings.Join(argv, " "))
		return nil, nil
	}
	cfg := Config{BinaryPath: "/usr/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installSystemdUnit(cfg, path, runner); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `ExecStart="/usr/bin/vmflow"`) {
		t.Fatalf("new unit was not installed:\n%s", got)
	}
	joined := "\n" + strings.Join(commands, "\n") + "\n"
	if !strings.Contains(joined, "\nsystemctl restart vmflow\n") {
		t.Fatalf("install did not restart the service: %v", commands)
	}
	if strings.Contains(joined, "\nsystemctl start vmflow\n") {
		t.Fatalf("install must restart, not start, an existing service: %v", commands)
	}
	if !strings.HasSuffix(joined, "systemctl is-active --quiet vmflow\n") {
		t.Fatalf("active verification was not the final install step: %v", commands)
	}
}

func TestInstallSystemdUnitRollsBackExistingUnitAndState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.service")
	oldUnit := []byte("old unit\n")
	if err := os.WriteFile(path, oldUnit, 0o640); err != nil {
		t.Fatal(err)
	}

	var commands []string
	restartCalls := 0
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		if command == "systemctl restart vmflow" {
			restartCalls++
			if restartCalls == 1 {
				return []byte("daemon failed"), errors.New("exit status 1")
			}
		}
		return nil, nil
	}
	cfg := Config{BinaryPath: "/usr/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"}
	err := installSystemdUnit(cfg, path, runner)
	if err == nil || !strings.Contains(err.Error(), "previous unit and service state restored") {
		t.Fatalf("expected a rolled-back install error, got %v", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldUnit) {
		t.Fatalf("old unit was not restored: got %q", got)
	}
	if restartCalls != 2 {
		t.Fatalf("expected failed new restart plus restored old restart, got %d; commands: %v", restartCalls, commands)
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("old unit mode was not restored: info=%v err=%v", info, statErr)
	}
}

func TestInstallSystemdUnitRollbackPreservesDisabledInactiveState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.service")
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var commands []string
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		switch command {
		case "systemctl is-enabled --quiet vmflow", "systemctl is-active --quiet vmflow":
			return nil, errors.New("inactive")
		case "systemctl restart vmflow":
			return []byte("daemon failed"), errors.New("exit status 1")
		default:
			return nil, nil
		}
	}
	cfg := Config{BinaryPath: "/usr/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installSystemdUnit(cfg, path, runner); err == nil {
		t.Fatal("expected restart failure")
	}

	joined := "\n" + strings.Join(commands, "\n") + "\n"
	if !strings.Contains(joined, "\nsystemctl disable vmflow\n") {
		t.Fatalf("rollback did not restore disabled state: %v", commands)
	}
	if !strings.HasSuffix(joined, "systemctl stop vmflow\n") {
		t.Fatalf("rollback did not restore inactive state: %v", commands)
	}
}

func TestInstallSystemdUnitRollbackRemovesNewUnitWhenNoneExisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.service")
	var commands []string
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		switch command {
		case "systemctl cat vmflow", "systemctl is-enabled --quiet vmflow":
			return nil, errors.New("unit not found")
		case "systemctl is-active --quiet vmflow":
			return []byte("inactive"), errors.New("exit status 3")
		default:
			return nil, nil
		}
	}
	cfg := Config{BinaryPath: "/usr/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installSystemdUnit(cfg, path, runner); err == nil {
		t.Fatal("expected active verification failure")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("new unit was left behind after rollback: %v", err)
	}
	joined := "\n" + strings.Join(commands, "\n") + "\n"
	if !strings.Contains(joined, "\nsystemctl disable vmflow\n") {
		t.Fatalf("rollback left the first-install enablement in place: %v", commands)
	}
}

// TestValidateTrustedServiceBinaryRejectsUserWritableTree is the core
// privilege-escalation defense: a binary reachable through a group/other-writable
// directory must be refused, so a non-root user cannot swap it after a privileged
// `sudo vmflow service install`. (Positive cases require a real root-owned install
// tree and are exercised by the end-to-end install test instead.)
func TestValidateTrustedServiceBinaryRejectsUserWritableTree(t *testing.T) {
	dir := t.TempDir()
	// Force the immediate parent to be group-writable so the rejection is
	// deterministic when the test runs as root (as root otherwise owns the tree).
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Skipf("cannot chmod temp dir: %v", err)
	}
	bin := filepath.Join(dir, "vmflow")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTrustedServiceBinary(bin, info); err == nil {
		t.Fatalf("expected rejection for binary in a group-writable tree")
	}
}
