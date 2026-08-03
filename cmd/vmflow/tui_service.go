package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/tui"
)

func localServiceControlEligible(address string, addressExplicit bool, getenv func(string) string) bool {
	if addressExplicit || strings.TrimSpace(getenv("VMFLOW_CONTROL_ADDR")) != "" || strings.TrimSpace(getenv("VMFLOW_CLIENT_CONFIG")) != "" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return false
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	host := strings.TrimSpace(parsed.Hostname())
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localTUIServiceReady(ctx context.Context, w io.Writer, translator *i18n.Translator, controller *tui.ServiceController) bool {
	if controller == nil || controller.Inspect == nil {
		return true
	}
	translator = translatorOrEnglish(translator)
	summary, err := controller.Inspect(ctx)
	if err != nil {
		// The management API remains the authoritative readiness check. If native
		// service inspection is inconclusive, let the TUI report the real error.
		return true
	}

	switch {
	case !summary.Installed && summary.State == "not installed":
		installCommand := strings.TrimSpace(controller.InstallCommand)
		if installCommand == "" {
			installCommand = nativeServiceInstallCommand("")
		}
		fmt.Fprintf(w, "%s\n\n%s\n  %s\n\n%s\n  vmflow tui\n",
			translator.T("cli.tui.service.not_installed"),
			translator.T("cli.tui.service.install_first"),
			installCommand,
			translator.T("cli.tui.service.retry"),
		)
		return false
	case summary.Installed && !summary.Running:
		fmt.Fprintf(w, "%s\n\n%s\n  %s\n\n%s\n  vmflow tui\n",
			translator.T("cli.tui.service.stopped"),
			translator.T("cli.tui.service.start_first"),
			nativeServiceStartCommand(),
			translator.T("cli.tui.service.retry"),
		)
		return false
	default:
		return true
	}
}

func newTUIServiceController(translator *i18n.Translator, configPath string) *tui.ServiceController {
	translator = translatorOrEnglish(translator)
	return &tui.ServiceController{
		Inspect: func(ctx context.Context) (tui.ServiceSummary, error) {
			if err := ctx.Err(); err != nil {
				return tui.ServiceSummary{}, err
			}
			summary, err := service.Inspect(service.Config{Translator: translator})
			return tui.ServiceSummary{
				Installed: summary.Installed,
				Running:   summary.Running,
				Enabled:   summary.Enabled,
				State:     summary.State,
				Detail:    summary.Detail,
			}, err
		},
		Start: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := service.Start(service.Config{Translator: translator}, io.Discard)
			switch {
			case errors.Is(err, service.ErrNotInstalled):
				return fmt.Errorf("%w: %v", tui.ErrServiceNotInstalled, err)
			case errors.Is(err, service.ErrPrivilegesRequired):
				return fmt.Errorf("%w: %v", tui.ErrServicePrivilegesRequired, err)
			default:
				return err
			}
		},
		ElevatedStartCommand: func() (*exec.Cmd, error) {
			return elevatedServiceStartCommand(translator.Locale())
		},
		InstallCommand: nativeServiceInstallCommand(configPath),
	}
}

func elevatedServiceStartCommand(locale string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve vmflow executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute vmflow executable: %w", err)
	}

	switch runtime.GOOS {
	case "windows":
		powershell, err := exec.LookPath("powershell.exe")
		if err != nil {
			return nil, fmt.Errorf("PowerShell is required for administrator startup: %w", err)
		}
		script := `$p = Start-Process -FilePath $args[0] -ArgumentList @('--lang', $args[1], 'service', 'start') -Verb RunAs -Wait -PassThru; exit $p.ExitCode`
		return exec.Command(powershell, "-NoProfile", "-Command", script, executable, locale), nil
	case "linux", "darwin":
		sudo, err := exec.LookPath("sudo")
		if err != nil {
			return nil, fmt.Errorf("sudo is required for administrator startup: %w", err)
		}
		return exec.Command(sudo, "--", executable, "--lang", locale, "service", "start"), nil
	default:
		return nil, fmt.Errorf("administrator startup is not supported on %s", runtime.GOOS)
	}
}

func nativeServiceInstallCommand(configPath string) string {
	command := "vmflow service install"
	if path := strings.TrimSpace(configPath); path != "" {
		if runtime.GOOS == "windows" {
			command += " --config " + strconv.Quote(path)
		} else {
			command += " --config '" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
		}
	}
	if runtime.GOOS != "windows" {
		command = "sudo " + command
	}
	return command
}

func nativeServiceStartCommand() string {
	command := "vmflow service start"
	if runtime.GOOS != "windows" {
		command = "sudo " + command
	}
	return command
}
