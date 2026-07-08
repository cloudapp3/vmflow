//go:build linux

package service

import (
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
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit missing %q\n---\n%s", want, unit)
		}
	}
	// ExecStart must reference the daemon and config, with config path quoted.
	if !strings.Contains(unit, "daemon") || !strings.Contains(unit, "-config") {
		t.Errorf("ExecStart missing daemon/-config:\n%s", unit)
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
	got := systemdExecStart(Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", LogFile: "/var/log/vmflow/v.log", ExtraArgs: "-control-listen 0.0.0.0:19090"})
	if !strings.Contains(got, `"/usr/local/bin/vmflow"`) {
		t.Errorf("binary path should be quoted: %s", got)
	}
	if !strings.Contains(got, "-log-file") {
		t.Errorf("expected -log-file when LogFile set: %s", got)
	}
	if !strings.Contains(got, "0.0.0.0:19090") {
		t.Errorf("expected extra-args passthrough: %s", got)
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
