//go:build linux

package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const (
	linuxUnitDir    = "/etc/systemd/system"
	linuxDefaultCfg = "/etc/vmflow/config.yaml"
)

func defaultConfigPath() string { return linuxDefaultCfg }

func unitPath(cfg Config) string {
	return filepath.Join(linuxUnitDir, cfg.ServiceName+".service")
}

func platformInstall(cfg Config, w io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing a systemd unit requires root (try sudo)")
	}
	if u := strings.TrimSpace(cfg.User); u != "" {
		if err := ensureSystemUser(u, w); err != nil {
			return err
		}
	}

	path := unitPath(cfg)
	if err := installSystemdUnit(cfg, path, runCombined); err != nil {
		return err
	}
	fmt.Fprintf(w, "installed %s\n", path)
	fmt.Fprintf(w, "service %s enabled and started\n", cfg.ServiceName)
	fmt.Fprintf(w, "logs: journalctl -u %s   |   status: vmflow service status\n", cfg.ServiceName)
	return nil
}

type systemdCommandRunner func([]string) ([]byte, error)

type unitFileSnapshot struct {
	exists     bool
	mode       os.FileMode
	data       []byte
	linkTarget string
}

type systemdState struct {
	known   bool
	enabled bool
	active  bool
}

// installSystemdUnit applies a unit update transactionally. A failed reload,
// enable, restart, or active-state check restores both the previous unit file
// and its enabled/running state as far as systemctl permits.
func installSystemdUnit(cfg Config, path string, runner systemdCommandRunner) error {
	previousUnit, err := snapshotUnitFile(path)
	if err != nil {
		return fmt.Errorf("snapshot existing unit file: %w", err)
	}
	previousState := snapshotSystemdState(cfg.ServiceName, runner)

	if err := writeFileAtomic(path, []byte(systemdUnit(cfg)), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	enableAttempted := false
	restartAttempted := false
	for _, step := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", cfg.ServiceName},
		{"systemctl", "restart", cfg.ServiceName},
		{"systemctl", "is-active", "--quiet", cfg.ServiceName},
	} {
		if len(step) > 1 && step[1] == "restart" {
			restartAttempted = true
		}
		if len(step) > 1 && step[1] == "enable" {
			enableAttempted = true
		}
		if err := runSystemdCommand(runner, step); err != nil {
			rollbackErr := rollbackSystemdInstall(path, previousUnit, previousState, cfg.ServiceName, enableAttempted, restartAttempted, runner)
			if rollbackErr != nil {
				return fmt.Errorf("%w; rollback incomplete: %v", err, rollbackErr)
			}
			return fmt.Errorf("%w; previous unit and service state restored", err)
		}
	}
	return nil
}

func snapshotUnitFile(path string) (unitFileSnapshot, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return unitFileSnapshot{}, nil
	}
	if err != nil {
		return unitFileSnapshot{}, err
	}

	snapshot := unitFileSnapshot{exists: true, mode: info.Mode()}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return unitFileSnapshot{}, err
		}
		snapshot.linkTarget = target
		return snapshot, nil
	}
	if !info.Mode().IsRegular() {
		return unitFileSnapshot{}, fmt.Errorf("existing unit path %s is not a regular file or symlink", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return unitFileSnapshot{}, err
	}
	snapshot.data = data
	return snapshot, nil
}

func restoreUnitFile(path string, snapshot unitFileSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if snapshot.mode&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(snapshot.linkTarget, path)
	}
	return writeFileAtomic(path, snapshot.data, snapshot.mode.Perm())
}

func snapshotSystemdState(serviceName string, runner systemdCommandRunner) systemdState {
	_, knownErr := runner([]string{"systemctl", "cat", serviceName})
	_, enabledErr := runner([]string{"systemctl", "is-enabled", "--quiet", serviceName})
	_, activeErr := runner([]string{"systemctl", "is-active", "--quiet", serviceName})
	return systemdState{
		known:   knownErr == nil || enabledErr == nil || activeErr == nil,
		enabled: enabledErr == nil,
		active:  activeErr == nil,
	}
}

func rollbackSystemdInstall(path string, previousUnit unitFileSnapshot, previousState systemdState, serviceName string, enableAttempted, restartAttempted bool, runner systemdCommandRunner) error {
	var problems []string
	if previousState.active || restartAttempted {
		if err := runSystemdCommand(runner, []string{"systemctl", "stop", serviceName}); err != nil {
			problems = append(problems, err.Error())
		}
	}
	// `systemctl enable` can create wants links before a later step fails. Remove
	// them while the new unit is still present unless the old service was enabled.
	if enableAttempted && !previousState.enabled {
		if err := runSystemdCommand(runner, []string{"systemctl", "disable", serviceName}); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if err := restoreUnitFile(path, previousUnit); err != nil {
		problems = append(problems, fmt.Sprintf("restore unit file: %v", err))
	}
	if err := runSystemdCommand(runner, []string{"systemctl", "daemon-reload"}); err != nil {
		problems = append(problems, err.Error())
	}
	if previousState.known {
		enableAction := "disable"
		if previousState.enabled {
			enableAction = "enable"
		}
		if err := runSystemdCommand(runner, []string{"systemctl", enableAction, serviceName}); err != nil {
			problems = append(problems, err.Error())
		}
		activeAction := "stop"
		if previousState.active {
			activeAction = "restart"
		}
		if err := runSystemdCommand(runner, []string{"systemctl", activeAction, serviceName}); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) != 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func runSystemdCommand(runner systemdCommandRunner, argv []string) error {
	out, err := runner(argv)
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
}

func platformUninstall(cfg Config, w io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("removing a systemd unit requires root (try sudo)")
	}
	// best-effort stop+disable; ignore errors if the service is not loaded
	_, _ = runCombined([]string{"systemctl", "stop", cfg.ServiceName})
	_, _ = runCombined([]string{"systemctl", "disable", cfg.ServiceName})

	path := unitPath(cfg)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}
	if out, err := runCombined([]string{"systemctl", "daemon-reload"}); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "service %s stopped and removed\n", cfg.ServiceName)
	fmt.Fprintln(w, "config and log files were left in place; remove them manually if desired")
	return nil
}

func platformStatus(cfg Config, w io.Writer) error {
	// `systemctl status` exits non-zero when the service is not running; we
	// surface its output regardless and swallow the exit code.
	cmd := exec.Command("systemctl", "status", cfg.ServiceName)
	cmd.Stdout = w
	cmd.Stderr = w
	_ = cmd.Run()
	return nil
}

func platformInspect(cfg Config) (Summary, error) {
	if info, err := os.Lstat(unitPath(cfg)); os.IsNotExist(err) {
		return Summary{State: "not installed"}, nil
	} else if err != nil {
		return Summary{State: "unknown"}, fmt.Errorf("inspect systemd unit: %w", err)
	} else if !info.Mode().IsRegular() {
		return Summary{State: "unknown", Detail: "unit path is not a regular file"}, nil
	}
	summary := Summary{Installed: true, State: "unknown"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	activeOutput, activeErr := exec.CommandContext(ctx, "systemctl", "is-active", cfg.ServiceName).CombinedOutput()
	if ctx.Err() != nil {
		return summary, fmt.Errorf("inspect systemd active state: %w", ctx.Err())
	}
	activeState := strings.TrimSpace(string(activeOutput))
	if activeState != "" {
		summary.State = activeState
	}
	summary.Running = activeErr == nil && activeState == "active"
	enabledOutput, enabledErr := exec.CommandContext(ctx, "systemctl", "is-enabled", cfg.ServiceName).CombinedOutput()
	if ctx.Err() != nil {
		return summary, fmt.Errorf("inspect systemd enabled state: %w", ctx.Err())
	}
	enabledState := strings.TrimSpace(string(enabledOutput))
	summary.Enabled = enabledErr == nil && (enabledState == "enabled" || enabledState == "static")
	if activeErr != nil && activeState == "" {
		summary.Detail = strings.TrimSpace(activeErr.Error())
	}
	return summary, nil
}

// ensureSystemUser creates name as a system (no-login) user if it does not
// already exist, so a non-root systemd unit can run under it.
func ensureSystemUser(name string, w io.Writer) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	out, err := runCombined([]string{"useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name})
	if err != nil {
		return fmt.Errorf("create system user %s: %w (%s); create it manually then retry", name, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "created system user %s\n", name)
	return nil
}
