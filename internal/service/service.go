// Package service registers vmflow as a native OS service so it starts at boot
// and restarts on crash:
//
//   - Linux:   systemd unit  (/etc/systemd/system/<name>.service)
//   - macOS:   launchd daemon (/Library/LaunchDaemons/io.cloudapp.<name>.plist)
//   - Windows: Windows Service (managed via services.msc / sc.exe)
//
// The package only performs install/uninstall/status: it generates the unit or
// plist file and invokes the platform's service manager (systemctl / launchctl /
// sc.exe). The daemon itself does not need this package to run — on Linux and
// macOS the service manager just execs `vmflow daemon` and supervises it via
// signals; on Windows the daemon detects the SCM at startup (see
// cmd/vmflow/daemon_windows.go) and reports state itself.
//
// Style follows internal/updater: error-return (no logger in the package),
// progress streamed to an io.Writer, and file writes use a same-directory temp
// + rename for atomicity.
package service

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultServiceName is the service/unit/registry name used when Config.Name is
// empty.
const DefaultServiceName = "vmflow"

// Config describes how to install or query the vmflow service.
type Config struct {
	// BinaryPath is the vmflow executable path. Defaults to the current
	// executable when empty.
	BinaryPath string
	// ConfigPath is the -config path the service runs with. Defaults to the
	// platform's system config path when empty.
	ConfigPath string
	// ServiceName is the service/unit/registry name. Defaults to "vmflow".
	ServiceName string
	// User is the systemd User= the unit runs as (Linux only). Empty = root.
	// When set and the account is missing, install creates it as a system user.
	User string
	// LogFile redirects daemon logs. On Linux/Windows it is passed to the daemon
	// via -log-file; on macOS it sets the launchd capture paths. Optional on
	// Linux/macOS, effectively required on Windows (the SCM provides no stdout).
	LogFile string
	// ExtraArgs are extra flags appended verbatim to the daemon command line,
	// e.g. "-control-listen 0.0.0.0:19090".
	ExtraArgs string
}

// normalize fills in platform-agnostic defaults (service name, binary path,
// config path) for a Config.
func normalize(cfg Config) Config {
	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = DefaultServiceName
	}
	if strings.TrimSpace(cfg.BinaryPath) == "" {
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				cfg.BinaryPath = resolved
			} else {
				cfg.BinaryPath = exe
			}
		}
	}
	if strings.TrimSpace(cfg.ConfigPath) == "" {
		cfg.ConfigPath = defaultConfigPath()
	}
	return cfg
}

// validateInstall checks install preconditions shared across platforms: the
// binary must be an absolute path in a trusted install location, and the config
// must already exist (a service with no working config only crash-loops).
func validateInstall(cfg Config) (Config, error) {
	binaryPath, err := trustedServiceBinaryPath(cfg.BinaryPath)
	if err != nil {
		return cfg, err
	}
	cfg.BinaryPath = binaryPath
	if _, err := os.Stat(cfg.ConfigPath); err != nil {
		return cfg, fmt.Errorf("config not found at %s: create it or pass --config <path>", cfg.ConfigPath)
	}
	return cfg, nil
}

// trustedServiceBinaryPath resolves binaryPath to the exact executable path the
// service manager will run and rejects paths that a less-privileged local user
// could replace after a privileged install. Platform-specific checks enforce
// root/admin ownership and non-writable path components.
func trustedServiceBinaryPath(binaryPath string) (string, error) {
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		return "", fmt.Errorf("could not determine vmflow binary path; pass --binary")
	}
	if !filepath.IsAbs(binaryPath) {
		return "", fmt.Errorf("binary path must be absolute: %s", binaryPath)
	}

	resolved, err := filepath.EvalSymlinks(binaryPath)
	if err != nil {
		return "", fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve binary path %s: %w", binaryPath, err)
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("binary not found at %s: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("binary path %s is not a regular file", resolved)
	}
	if err := validateTrustedServiceBinary(resolved, info); err != nil {
		return "", err
	}
	return resolved, nil
}

// Install registers, enables, and starts the service.
func Install(cfg Config, w io.Writer) error {
	cfg = normalize(cfg)
	var err error
	cfg, err = validateInstall(cfg)
	if err != nil {
		return err
	}
	return platformInstall(cfg, w)
}

// Uninstall stops and removes the service. Config and log files are left in
// place for the operator to clean up.
func Uninstall(cfg Config, w io.Writer) error {
	cfg = normalize(cfg)
	return platformUninstall(cfg, w)
}

// Status prints the current service status to w.
func Status(cfg Config, w io.Writer) error {
	cfg = normalize(cfg)
	return platformStatus(cfg, w)
}

// writeFileAtomic writes data to path via a same-directory temp file + rename,
// matching internal/updater's safe-write pattern. The target directory must
// already exist and be writable (root for system paths).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".vmflow-svc-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// runCombined runs argv and returns its combined output. Shared by the
// platform installers so command construction stays testable.
func runCombined(argv []string) ([]byte, error) {
	return exec.Command(argv[0], argv[1:]...).CombinedOutput()
}

// shellQuote wraps s in double quotes, escaping embedded quotes. systemd's
// ExecStart and macOS do not run a shell, but both parse double quotes, so
// quoting keeps paths with spaces intact.
func shellQuote(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}
