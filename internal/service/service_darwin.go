//go:build darwin

package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/internal/appdirs"
)

const (
	darwinPlistDir   = "/Library/LaunchDaemons"
	darwinDefaultCfg = appdirs.DarwinSystemConfigPath
	darwinLogDir     = "/var/log/vmflow"
)

func plistPath(cfg Config) string {
	return filepath.Join(darwinPlistDir, launchdLabel(cfg)+".plist")
}

func platformInstall(cfg Config, w io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing a launchd daemon requires root (try sudo)")
	}
	if err := os.MkdirAll(darwinLogDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	path := plistPath(cfg)
	if err := installLaunchdDaemon(cfg, path, runCombined); err != nil {
		return err
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.installed", path))

	label := launchdLabel(cfg)
	fmt.Fprintln(w, cfg.tf("cli.service.output.daemon_started", label))
	fmt.Fprintln(w, cfg.tf("cli.service.output.darwin_logs", logStdout(cfg), logStderr(cfg)))
	return nil
}

func platformStart(cfg Config, w io.Writer) error {
	path := plistPath(cfg)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrNotInstalled, cfg.ServiceName)
	}
	if err != nil {
		return fmt.Errorf("inspect launchd plist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("launchd plist path %s is not a regular file", path)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w (try sudo)", ErrPrivilegesRequired)
	}
	if err := startLaunchdDaemon(cfg, path, runCombined); err != nil {
		return err
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.started", cfg.ServiceName))
	return nil
}

func platformUninstall(cfg Config, w io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("removing a launchd daemon requires root (try sudo)")
	}
	label := launchdLabel(cfg)
	if err := uninstallLaunchdDaemon(cfg, plistPath(cfg), runCombined); err != nil {
		return err
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.daemon_removed", label))
	fmt.Fprintln(w, cfg.t("cli.service.output.files_preserved"))
	return nil
}

func platformStatus(cfg Config, w io.Writer) error {
	cmd := exec.Command("launchctl", "print", "system/"+launchdLabel(cfg))
	cmd.Stdout = w
	cmd.Stderr = w
	_ = cmd.Run()
	return nil
}

func platformInspect(cfg Config) (Summary, error) {
	if info, err := os.Lstat(plistPath(cfg)); os.IsNotExist(err) {
		return Summary{State: "not installed"}, nil
	} else if err != nil {
		return Summary{State: "unknown"}, fmt.Errorf("inspect launchd plist: %w", err)
	} else if !info.Mode().IsRegular() {
		return Summary{State: "unknown", Detail: "plist path is not a regular file"}, nil
	}
	summary := Summary{Installed: true, Enabled: true, State: "loaded"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "launchctl", "print", "system/"+launchdLabel(cfg)).CombinedOutput()
	if ctx.Err() != nil {
		return summary, fmt.Errorf("inspect launchd state: %w", ctx.Err())
	}
	text := strings.ToLower(string(output))
	if err == nil {
		summary.Running = strings.Contains(text, "state = running")
		if summary.Running {
			summary.State = "running"
		}
	} else {
		summary.State = "not loaded"
		summary.Detail = strings.TrimSpace(string(output))
	}
	return summary, nil
}

func logStdout(cfg Config) string { s, _ := launchdLogPaths(cfg); return s }
func logStderr(cfg Config) string { _, s := launchdLogPaths(cfg); return s }
