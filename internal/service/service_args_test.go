package service

import (
	"strings"
	"testing"
)

func TestValidateServiceExtraArgsRejectsReservedAndPositionalTokens(t *testing.T) {
	tests := []string{
		"-config", "--config=other.yaml",
		"-log-file=other.log", "--control-port", "--control-listen", "-insecure-allow-remote-control=true",
		"--service-name=other", "--", "positional",
	}
	for _, arg := range tests {
		t.Run(strings.ReplaceAll(arg, "/", "_"), func(t *testing.T) {
			if err := validateServiceExtraArgs([]string{arg}); err == nil {
				t.Fatalf("validateServiceExtraArgs(%q) succeeded, want rejection", arg)
			}
		})
	}
}

func TestValidateServiceExtraArgsAllowsFutureFlagTokens(t *testing.T) {
	args := []string{"-future-switch", `--future-value=value with spaces and "quotes"`, ""}
	if err := validateServiceExtraArgs(args); err != nil {
		t.Fatalf("validateServiceExtraArgs: %v", err)
	}
}

func TestForegroundArgsUseDirectRuntimeEntry(t *testing.T) {
	cfg := Config{ConfigPath: "/etc/vmflow/config.yaml"}
	args := foregroundArgs(cfg, false)
	if len(args) != 2 || args[0] != "-config" || args[1] != cfg.ConfigPath {
		t.Fatalf("foregroundArgs = %#v, want direct -config arguments", args)
	}
	for _, arg := range args {
		if arg == "daemon" || arg == "d" {
			t.Fatalf("removed daemon command leaked into service arguments: %#v", args)
		}
	}
}
