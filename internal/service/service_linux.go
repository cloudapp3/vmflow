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

	"github.com/cloudapp3/vmflow/internal/appdirs"
)

const (
	linuxUnitDir    = "/etc/systemd/system"
	linuxDefaultCfg = appdirs.LinuxSystemConfigPath
)

func unitPath(cfg Config) string {
	return filepath.Join(linuxUnitDir, cfg.ServiceName+".service")
}

func platformInstall(cfg Config, w io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing a systemd unit requires root (try sudo)")
	}
	if u := strings.TrimSpace(cfg.User); u != "" {
		if err := ensureSystemUser(u, w, cfg); err != nil {
			return err
		}
	}

	path := unitPath(cfg)
	if err := installSystemdUnit(cfg, path, runCombined); err != nil {
		return err
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.installed", path))
	fmt.Fprintln(w, cfg.tf("cli.service.output.enabled_started", cfg.ServiceName))
	fmt.Fprintln(w, cfg.tf("cli.service.output.linux_logs", cfg.ServiceName))
	return nil
}

func platformStart(cfg Config, w io.Writer) error {
	info, err := os.Lstat(unitPath(cfg))
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrNotInstalled, cfg.ServiceName)
	}
	if err != nil {
		return fmt.Errorf("inspect systemd unit: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("systemd unit path %s is not a regular file", unitPath(cfg))
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w (try sudo)", ErrPrivilegesRequired)
	}
	if err := startSystemdService(cfg.ServiceName, runCombined); err != nil {
		return err
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.started", cfg.ServiceName))
	return nil
}

func startSystemdService(serviceName string, runner systemdCommandRunner) error {
	for _, step := range [][]string{
		{"systemctl", "start", serviceName},
		{"systemctl", "is-active", "--quiet", serviceName},
	} {
		if err := runSystemdCommand(runner, step); err != nil {
			return err
		}
	}
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
	if err := uninstallSystemdService(cfg.ServiceName, unitPath(cfg), runCombined); err != nil {
		return err
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.stopped_removed", cfg.ServiceName))
	fmt.Fprintln(w, cfg.t("cli.service.output.files_preserved"))
	return nil
}

func uninstallSystemdService(serviceName, path string, runner systemdCommandRunner) error {
	_, pathErr := os.Lstat(path)
	if pathErr != nil && !os.IsNotExist(pathErr) {
		return fmt.Errorf("inspect systemd unit: %w", pathErr)
	}
	state, err := systemdActiveState(serviceName, runner)
	if err != nil {
		if os.IsNotExist(pathErr) {
			return nil
		}
		return err
	}
	if os.IsNotExist(pathErr) && systemdStoppedState(state) {
		return nil
	}

	if !systemdStoppedState(state) {
		if err := runSystemdCommand(runner, []string{"systemctl", "stop", serviceName}); err != nil {
			return err
		}
		state, err = systemdActiveState(serviceName, runner)
		if err != nil {
			return fmt.Errorf("verify systemd service stopped: %w", err)
		}
		if !systemdStoppedState(state) {
			return fmt.Errorf("systemd service %s did not stop (active state %q)", serviceName, state)
		}
	}

	if err := runSystemdCommand(runner, []string{"systemctl", "disable", serviceName}); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}
	if err := runSystemdCommand(runner, []string{"systemctl", "daemon-reload"}); err != nil {
		return err
	}
	return nil
}

func systemdActiveState(serviceName string, runner systemdCommandRunner) (string, error) {
	argv := []string{"systemctl", "is-active", serviceName}
	out, err := runner(argv)
	state := strings.TrimSpace(string(out))
	switch state {
	case "active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing", "unknown":
		return state, nil
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, state)
	}
	return "", fmt.Errorf("%s returned unexpected active state %q", strings.Join(argv, " "), state)
}

func systemdStoppedState(state string) bool {
	return state == "inactive" || state == "failed" || state == "unknown"
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
func ensureSystemUser(name string, w io.Writer, cfg Config) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	out, err := runCombined([]string{"useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name})
	if err != nil {
		return fmt.Errorf("create system user %s: %w (%s); create it manually then retry", name, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintln(w, cfg.tf("cli.service.output.user_created", name))
	return nil
}
