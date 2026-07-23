//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsDefaultCfg        = `C:\ProgramData\vmflow\config.yaml`
	windowsDefaultLog        = `C:\ProgramData\vmflow\logs\vmflow.log`
	windowsServiceTimeout    = 30 * time.Second
	windowsServicePollPeriod = 250 * time.Millisecond
	windowsServiceDesc       = "vmflow L4 forwarding daemon"
	windowsRecoveryResetSecs = 24 * 60 * 60
)

func defaultConfigPath() string { return windowsDefaultCfg }

func platformInstall(cfg Config, w io.Writer) error {
	if strings.TrimSpace(cfg.LogFile) == "" {
		// The SCM provides no stdout, so always give the service a durable log.
		cfg.LogFile = windowsDefaultLog
	}
	args := windowsServiceArgs(cfg)
	desiredCommandLine := windows.ComposeCommandLine(append([]string{cfg.BinaryPath}, args...))

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(cfg.ServiceName)
	created := false
	var previous *windowsServiceSnapshot
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		s, err = m.CreateService(cfg.ServiceName, cfg.BinaryPath, mgr.Config{
			ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
			StartType:    mgr.StartAutomatic,
			ErrorControl: mgr.ErrorNormal,
			DisplayName:  cfg.ServiceName,
			Description:  windowsServiceDesc,
		}, args...)
		if err != nil {
			return fmt.Errorf("create Windows service %s: %w", cfg.ServiceName, err)
		}
		created = true
	} else if err != nil {
		return fmt.Errorf("open Windows service %s: %w", cfg.ServiceName, err)
	} else {
		current, configErr := snapshotWindowsService(s)
		if configErr != nil {
			s.Close()
			return fmt.Errorf("snapshot Windows service %s: %w", cfg.ServiceName, configErr)
		}
		previous = &current
		// Reinstall is also the supported way to apply config-file content
		// changes, so always restart an existing instance even when ImagePath is
		// unchanged.
		if stopErr := stopWindowsService(s, windowsServiceTimeout); stopErr != nil {
			rollbackErr := restoreWindowsService(s, current)
			s.Close()
			return errors.Join(fmt.Errorf("stop Windows service %s before reconfiguration: %w", cfg.ServiceName, stopErr), rollbackErr)
		}

		// Preserve account, dependencies, load-order group, and SID choices while
		// replacing the executable command and enforcing automatic startup.
		updated := desiredWindowsServiceConfig(current.Config, cfg.ServiceName, desiredCommandLine)
		if configErr = s.UpdateConfig(updated); configErr != nil {
			rollbackErr := restoreWindowsService(s, current)
			s.Close()
			return errors.Join(fmt.Errorf("update Windows service %s: %w", cfg.ServiceName, configErr), rollbackErr)
		}
	}
	defer s.Close()

	recovery := desiredWindowsRecoveryPolicy()
	if err := s.SetRecoveryActions(recovery.Actions, recovery.ResetPeriod); err != nil {
		return rollbackWindowsInstall(s, cfg.ServiceName, created, previous, fmt.Errorf("configure Windows service %s recovery: %w", cfg.ServiceName, err))
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(recovery.NonCrashFailures); err != nil {
		return rollbackWindowsInstall(s, cfg.ServiceName, created, previous, fmt.Errorf("enable Windows service %s non-crash recovery: %w", cfg.ServiceName, err))
	}

	if err := ensureWindowsServiceRunning(s, windowsServiceTimeout); err != nil {
		return rollbackWindowsInstall(s, cfg.ServiceName, created, previous, fmt.Errorf("start Windows service %s: %w", cfg.ServiceName, err))
	}
	verb := "updated"
	if created {
		verb = "created"
	} else {
		fmt.Fprintf(w, "service %s already exists; configuration updated\n", cfg.ServiceName)
	}
	fmt.Fprintf(w, "service %s %s and running (start=automatic, restart on failure)\n", cfg.ServiceName, verb)
	fmt.Fprintf(w, "logs: %s   |   status: vmflow service status\n", cfg.LogFile)
	return nil
}

type windowsServiceSnapshot struct {
	Config                 mgr.Config
	RecoveryActions        []mgr.RecoveryAction
	RecoveryResetPeriod    uint32
	RecoverNonCrashFailure bool
	ShouldRun              bool
}

type windowsRecoveryPolicy struct {
	Actions          []mgr.RecoveryAction
	ResetPeriod      uint32
	NonCrashFailures bool
}

func desiredWindowsServiceConfig(current mgr.Config, name, commandLine string) mgr.Config {
	current.ServiceType = windows.SERVICE_WIN32_OWN_PROCESS
	current.StartType = mgr.StartAutomatic
	current.ErrorControl = mgr.ErrorNormal
	current.BinaryPathName = commandLine
	current.DisplayName = name
	current.Description = windowsServiceDesc
	current.DelayedAutoStart = false
	current.ServiceStartName = "" // nil means keep the existing account
	current.Password = ""
	return current
}

func desiredWindowsRecoveryPolicy() windowsRecoveryPolicy {
	return windowsRecoveryPolicy{
		Actions: []mgr.RecoveryAction{{
			Type:  mgr.ServiceRestart,
			Delay: 5 * time.Second,
		}},
		ResetPeriod:      windowsRecoveryResetSecs,
		NonCrashFailures: true,
	}
}

func snapshotWindowsService(s *mgr.Service) (windowsServiceSnapshot, error) {
	serviceCfg, err := s.Config()
	if err != nil {
		return windowsServiceSnapshot{}, err
	}
	status, err := s.Query()
	if err != nil {
		return windowsServiceSnapshot{}, err
	}
	recoveryActions, err := s.RecoveryActions()
	if err != nil {
		return windowsServiceSnapshot{}, err
	}
	resetPeriod, err := s.ResetPeriod()
	if err != nil {
		return windowsServiceSnapshot{}, err
	}
	recoverNonCrash, err := s.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return windowsServiceSnapshot{}, err
	}
	shouldRun := status.State != svc.Stopped && status.State != svc.StopPending
	return windowsServiceSnapshot{
		Config:                 serviceCfg,
		RecoveryActions:        recoveryActions,
		RecoveryResetPeriod:    resetPeriod,
		RecoverNonCrashFailure: recoverNonCrash,
		ShouldRun:              shouldRun,
	}, nil
}

func rollbackWindowsInstall(s *mgr.Service, name string, created bool, previous *windowsServiceSnapshot, installErr error) error {
	var rollbackErr error
	if created {
		if err := stopWindowsService(s, windowsServiceTimeout); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("stop newly created service during rollback: %w", err))
		}
		if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("delete newly created service during rollback: %w", err))
		}
	} else if previous != nil {
		rollbackErr = restoreWindowsService(s, *previous)
	}
	if rollbackErr != nil {
		return errors.Join(installErr, fmt.Errorf("rollback Windows service %s: %w", name, rollbackErr))
	}
	return installErr
}

func restoreWindowsService(s *mgr.Service, snapshot windowsServiceSnapshot) error {
	var rollbackErr error
	if err := stopWindowsService(s, windowsServiceTimeout); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("stop updated service: %w", err))
	}
	previousConfig := snapshot.Config
	// The account was never changed, so leave it untouched instead of asking
	// ChangeServiceConfig to revalidate an account without its password.
	previousConfig.ServiceStartName = ""
	previousConfig.Password = ""
	if err := s.UpdateConfig(previousConfig); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore service config: %w", err))
	}
	if len(snapshot.RecoveryActions) == 0 {
		if err := s.ResetRecoveryActions(); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("reset recovery actions: %w", err))
		}
	} else if err := s.SetRecoveryActions(snapshot.RecoveryActions, snapshot.RecoveryResetPeriod); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore recovery actions: %w", err))
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(snapshot.RecoverNonCrashFailure); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore non-crash recovery flag: %w", err))
	}
	if snapshot.ShouldRun {
		if err := ensureWindowsServiceRunning(s, windowsServiceTimeout); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore running state: %w", err))
		}
	}
	return rollbackErr
}

func platformUninstall(cfg Config, w io.Writer) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(cfg.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		fmt.Fprintf(w, "service %s is not installed\n", cfg.ServiceName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Windows service %s: %w", cfg.ServiceName, err)
	}
	defer s.Close()

	if err := stopWindowsService(s, windowsServiceTimeout); err != nil {
		return fmt.Errorf("stop Windows service %s: %w", cfg.ServiceName, err)
	}
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("delete Windows service %s: %w", cfg.ServiceName, err)
	}
	fmt.Fprintf(w, "service %s stopped and removed\n", cfg.ServiceName)
	fmt.Fprintln(w, "config and log files were left in place; remove them manually if desired")
	return nil
}

func platformStatus(cfg Config, w io.Writer) error {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fmt.Errorf("connect to Windows service manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	name, err := windows.UTF16PtrFromString(cfg.ServiceName)
	if err != nil {
		return fmt.Errorf("invalid Windows service name %q: %w", cfg.ServiceName, err)
	}
	handle, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return fmt.Errorf("open Windows service %s: %w", cfg.ServiceName, err)
	}
	s := &mgr.Service{Name: cfg.ServiceName, Handle: handle}
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("query Windows service %s: %w", cfg.ServiceName, err)
	}
	serviceCfg, err := s.Config()
	if err != nil {
		return fmt.Errorf("read Windows service %s config: %w", cfg.ServiceName, err)
	}

	fmt.Fprintf(w, "service: %s\nstate: %s\nstartup: %s\npid: %d\n", cfg.ServiceName, windowsServiceStateName(status.State), windowsServiceStartTypeName(serviceCfg.StartType), status.ProcessId)
	if status.State == svc.Stopped && (status.Win32ExitCode != 0 || status.ServiceSpecificExitCode != 0) {
		fmt.Fprintf(w, "last exit: win32=%d service=%d\n", status.Win32ExitCode, status.ServiceSpecificExitCode)
	}
	return nil
}

func platformInspect(cfg Config) (Summary, error) {
	m, err := mgr.Connect()
	if err != nil {
		return Summary{State: "unknown"}, fmt.Errorf("connect to Windows service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(cfg.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return Summary{State: "not installed"}, nil
	}
	if err != nil {
		return Summary{State: "unknown"}, fmt.Errorf("open Windows service %s: %w", cfg.ServiceName, err)
	}
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return Summary{Installed: true, State: "unknown"}, fmt.Errorf("query Windows service %s: %w", cfg.ServiceName, err)
	}
	serviceCfg, err := s.Config()
	if err != nil {
		return Summary{Installed: true, State: windowsServiceStateName(status.State)}, fmt.Errorf("read Windows service %s config: %w", cfg.ServiceName, err)
	}
	return Summary{
		Installed: true,
		Running:   status.State == svc.Running,
		Enabled:   serviceCfg.StartType != mgr.StartDisabled,
		State:     windowsServiceStateName(status.State),
	}, nil
}

func windowsServiceArgs(cfg Config) []string {
	args := foregroundArgs(cfg, true)
	// svc.Run must register the exact CreateService name for custom names.
	return append(args, "-service-name", cfg.ServiceName)
}

func ensureWindowsServiceRunning(s *mgr.Service, timeout time.Duration) error {
	status, err := s.Query()
	if err != nil {
		return err
	}
	switch status.State {
	case svc.Running:
		return nil
	case svc.StopPending:
		if _, err := waitWindowsServiceState(s, svc.Stopped, timeout); err != nil {
			return err
		}
	case svc.Stopped:
		// Ready to start below.
	case svc.StartPending:
		_, err := waitWindowsServiceState(s, svc.Running, timeout)
		return err
	default:
		return fmt.Errorf("cannot start service from state %s", windowsServiceStateName(status.State))
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	_, err = waitWindowsServiceState(s, svc.Running, timeout)
	return err
}

func stopWindowsService(s *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stopSent := false
	for {
		status, err := s.Query()
		if err != nil {
			return err
		}
		switch status.State {
		case svc.Stopped:
			return nil
		case svc.StopPending:
			stopSent = true
		case svc.Running, svc.Paused:
			if !stopSent {
				if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
					return err
				}
				stopSent = true
			}
		case svc.StartPending, svc.ContinuePending, svc.PausePending:
			// Wait for a stable state before requesting stop.
		default:
			return fmt.Errorf("cannot stop service from unknown state %d", status.State)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for stopped state (last state %s)", timeout, windowsServiceStateName(status.State))
		}
		time.Sleep(windowsServicePollPeriod)
	}
}

func waitWindowsServiceState(s *mgr.Service, target svc.State, timeout time.Duration) (svc.Status, error) {
	deadline := time.Now().Add(timeout)
	var last svc.Status
	for {
		status, err := s.Query()
		if err != nil {
			return status, err
		}
		last = status
		if status.State == target {
			return status, nil
		}
		if target == svc.Running && status.State == svc.Stopped {
			return status, fmt.Errorf("service stopped during startup (win32 exit=%d, service exit=%d)", status.Win32ExitCode, status.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out after %s waiting for %s (last state %s)", timeout, windowsServiceStateName(target), windowsServiceStateName(last.State))
		}
		time.Sleep(windowsServicePollPeriod)
	}
}

func windowsServiceStateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start-pending"
	case svc.StopPending:
		return "stop-pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue-pending"
	case svc.PausePending:
		return "pause-pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func windowsServiceStartTypeName(startType uint32) string {
	switch startType {
	case mgr.StartAutomatic:
		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	default:
		return fmt.Sprintf("unknown(%d)", startType)
	}
}
