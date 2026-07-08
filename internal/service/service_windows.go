//go:build windows

package service

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const windowsDefaultCfg = `C:\ProgramData\vmflow\config.yaml`

func defaultConfigPath() string { return windowsDefaultCfg }

// ERROR_SERVICE_EXISTS (1073) from sc.exe when the service is already present.
const scErrServiceExists = "1073"

func platformInstall(cfg Config, w io.Writer) error {
	// sc.exe requires administrator privileges; let it report a clear error if
	// the caller is not elevated.
	binPath := scBinPath(cfg)
	if lp := strings.TrimSpace(cfg.LogFile); lp == "" {
		// Under the SCM there is no stdout, so default logs to the standard
		// ProgramData location unless overridden.
		cfg.LogFile = `C:\ProgramData\vmflow\logs\vmflow.log`
		binPath = scBinPath(cfg)
	}

	create := []string{"create", cfg.ServiceName, "binPath=", binPath, "start=", "auto"}
	if out, err := runCombined(append([]string{"sc.exe"}, create...)); err != nil {
		// tolerate "service already exists"
		if !strings.Contains(string(out), scErrServiceExists) {
			return fmt.Errorf("sc create: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(w, "service %s already exists; reconfiguring\n", cfg.ServiceName)
	}

	for _, step := range [][]string{
		{"description", cfg.ServiceName, "vmflow L4 forwarding daemon"},
		{"failure", cfg.ServiceName, "reset=", "0", "actions=", "restart/5000"},
	} {
		if out, err := runCombined(append([]string{"sc.exe"}, step...)); err != nil {
			return fmt.Errorf("sc %s: %w (%s)", step[0], err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Fprintf(w, "service %s created (start=auto, restart on failure)\n", cfg.ServiceName)

	if out, err := runCombined([]string{"sc.exe", "start", cfg.ServiceName}); err != nil {
		fmt.Fprintf(w, "note: sc start: %s (it will still start at boot)\n", strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "logs: %s   |   status: vmflow service status (or: sc query %s)\n", cfg.LogFile, cfg.ServiceName)
	return nil
}

func platformUninstall(cfg Config, w io.Writer) error {
	_, _ = runCombined([]string{"sc.exe", "stop", cfg.ServiceName})
	out, err := runCombined([]string{"sc.exe", "delete", cfg.ServiceName})
	if err != nil {
		return fmt.Errorf("sc delete: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "service %s removed\n", cfg.ServiceName)
	fmt.Fprintln(w, "config and log files were left in place; remove them manually if desired")
	return nil
}

func platformStatus(cfg Config, w io.Writer) error {
	cmd := exec.Command("sc.exe", "query", cfg.ServiceName)
	cmd.Stdout = w
	cmd.Stderr = w
	_ = cmd.Run()
	return nil
}
