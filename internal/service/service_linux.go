//go:build linux

package service

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
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
	if err := writeFileAtomic(path, []byte(systemdUnit(cfg)), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}
	fmt.Fprintf(w, "installed %s\n", path)

	for _, step := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", cfg.ServiceName},
		{"systemctl", "start", cfg.ServiceName},
	} {
		out, err := runCombined(step)
		if err != nil {
			return fmt.Errorf("%s: %w (%s)", strings.Join(step, " "), err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Fprintf(w, "service %s enabled and started\n", cfg.ServiceName)
	fmt.Fprintf(w, "logs: journalctl -u %s   |   status: vmflow service status\n", cfg.ServiceName)
	return nil
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
