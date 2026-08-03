package main

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
)

type initServicePayload struct {
	BinaryPath  string                       `json:"binary_path"`
	ConfigPath  string                       `json:"config_path"`
	ServiceName string                       `json:"service_name"`
	Locale      string                       `json:"locale"`
	Update      controlapi.SetupConfigUpdate `json:"update"`
}

type initServiceOps struct {
	elevated    bool
	install     func(initServicePayload, io.Writer) error
	runElevated func(initServicePayload, string, io.Writer) error
	inspect     func(service.Config) (service.Summary, error)
	waitReady   func(string) error
}

func prepareInitServiceInstall(configPath string, translator *i18n.Translator) (service.InstallPlan, error) {
	return service.PlanInstallTarget(service.Config{
		Translator: translatorOrEnglish(translator),
		ConfigPath: configPath,
	})
}

func printInitServicePlan(output io.Writer, plan service.InstallPlan, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.T("cli.init.service.plan.title"))
	fmt.Fprintln(output, translator.Tf("cli.init.service.plan.binary", plan.BinaryPath))
	fmt.Fprintln(output, translator.Tf("cli.init.service.plan.config", plan.ConfigPath))
	fmt.Fprintln(output, translator.T("cli.init.service.plan.behavior"))
}

func runInitService(result initResult, output io.Writer, translator *i18n.Translator) error {
	return runInitServiceWithOps(result, output, translator, initServiceOps{
		elevated:    currentProcessElevated(),
		install:     installInitSystemService,
		runElevated: runElevatedInitServiceInstall,
		inspect:     service.Inspect,
		waitReady:   waitForInitServiceReady,
	})
}

func runInitServiceWithOps(result initResult, output io.Writer, translator *i18n.Translator, ops initServiceOps) error {
	translator = translatorOrEnglish(translator)
	plan := result.ServicePlan
	payload := initServicePayload{
		BinaryPath:  plan.BinaryPath,
		ConfigPath:  plan.ConfigPath,
		ServiceName: plan.ServiceName,
		Locale:      translator.Locale(),
		Update:      result.setupUpdate,
	}
	cfg := service.Config{
		Translator:  translator,
		BinaryPath:  plan.BinaryPath,
		ConfigPath:  plan.ConfigPath,
		ServiceName: plan.ServiceName,
	}
	if ops.elevated {
		if ops.install == nil {
			return fmt.Errorf("native service installer is unavailable")
		}
		if err := ops.install(payload, output); err != nil {
			return err
		}
	} else {
		if ops.runElevated == nil {
			return fmt.Errorf("administrator service installer is unavailable")
		}
		if err := ops.runElevated(payload, translator.Locale(), output); err != nil {
			return err
		}
	}

	if ops.inspect == nil {
		return fmt.Errorf("native service inspection is unavailable")
	}
	summary, err := ops.inspect(cfg)
	if err != nil {
		return savedInitServiceError(fmt.Errorf("verify native service: %w", err))
	}
	if !summary.Installed || !summary.Running || !summary.Enabled {
		detail := strings.TrimSpace(summary.Detail)
		if detail == "" {
			detail = summary.State
		}
		return savedInitServiceError(fmt.Errorf("native service is not ready (installed=%t running=%t enabled=%t state=%s)", summary.Installed, summary.Running, summary.Enabled, detail))
	}
	if ops.waitReady == nil {
		return savedInitServiceError(fmt.Errorf("management readiness check is unavailable"))
	}
	if err := ops.waitReady(result.ControlAddress); err != nil {
		return savedInitServiceError(err)
	}

	printInitServiceReady(output, result, plan.ServiceName, translator)
	return nil
}

func waitForInitServiceReady(address string) error {
	deadline := time.Now().Add(10 * time.Second)
	var last daemonProbe
	for {
		last = probeDaemon(address, "", clientTLSOptionsFromEnv(), nil, 500*time.Millisecond)
		if last.Status == "running" {
			return nil
		}
		if time.Now().After(deadline) {
			detail := strings.TrimSpace(last.Detail)
			if detail == "" {
				detail = last.Status
			}
			return fmt.Errorf("system service is active but management endpoint %s is not ready: %s", address, detail)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func printInitServiceReady(output io.Writer, result initResult, serviceName string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	if strings.TrimSpace(serviceName) == "" {
		serviceName = service.DefaultServiceName
	}
	fmt.Fprintln(output, translator.T("cli.init.service.ready"))
	fmt.Fprintln(output, translator.Tf("cli.init.service.state", serviceName))
	fmt.Fprintln(output, translator.Tf("cli.output.config_scope", translator.T("cli.output.config_scope.system")))
	fmt.Fprintln(output, translator.Tf("cli.output.config", result.ConfigPath))
	if strings.TrimSpace(result.Language) != "" {
		fmt.Fprintln(output, translator.Tf("cli.init.language_summary", result.Language))
	}
	if strings.TrimSpace(result.Rule.RuleID) != "" {
		fmt.Fprintln(output, translator.Tf("cli.init.rule_summary", result.Rule.RuleID, result.Rule.Protocol, result.Rule.ListenAddr, result.Rule.ListenPort, result.Rule.TargetAddr, result.Rule.TargetPort))
	}
	if result.ProfilePath != "" {
		fmt.Fprintln(output, translator.Tf("cli.init.management_profile", result.ProfilePath))
	} else if result.preparedConfig.Version != 0 && !result.preparedConfig.Auth.Enabled {
		fmt.Fprintln(output, translator.T("cli.init.management_read_only"))
	}
	fmt.Fprintf(output, "%s: %s\n", translator.T("cli.status.label.control"), result.ControlAddress)
	fmt.Fprintln(output, translator.T("cli.init.service.dashboard"))
	fmt.Fprintln(output, translator.T("cli.init.service.status"))
	switch runtime.GOOS {
	case "linux":
		fmt.Fprintln(output, translator.Tf("cli.init.service.logs.linux", serviceName))
	case "darwin":
		fmt.Fprintln(output, translator.T("cli.init.service.logs.darwin"))
	case "windows":
		fmt.Fprintln(output, translator.T("cli.init.service.logs.windows"))
	}
}

func printInitServiceFailure(output io.Writer, result initResult, translator *i18n.Translator, err error) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.Tf("cli.init.service.failed", err))
	if initServiceConfigSaved(err) {
		fmt.Fprintln(output, translator.Tf("cli.init.service.config_saved", result.ConfigPath))
		fmt.Fprintln(output, translator.Tf("cli.init.service.retry", nativeServiceInstallCommand(result.ConfigPath)))
	} else {
		fmt.Fprintln(output, translator.T("cli.init.service.config_rolled_back"))
		fmt.Fprintln(output, translator.T("cli.init.service.retry_init"))
	}
}

type initServiceInstallError struct {
	err         error
	configSaved bool
}

func (err *initServiceInstallError) Error() string { return err.err.Error() }
func (err *initServiceInstallError) Unwrap() error { return err.err }

func savedInitServiceError(err error) error {
	if err == nil || initServiceConfigSaved(err) {
		return err
	}
	return &initServiceInstallError{err: err, configSaved: true}
}

func unsavedInitServiceError(err error) error {
	if err == nil {
		return nil
	}
	return &initServiceInstallError{err: err, configSaved: false}
}

func initServiceConfigSaved(err error) bool {
	var installErr *initServiceInstallError
	return errors.As(err, &installErr) && installErr.configSaved
}

func installInitSystemService(payload initServicePayload, output io.Writer) error {
	return installInitSystemServiceWithOps(payload, output, initSystemInstallOps{
		elevated: currentProcessElevated(),
		plan:     service.PlanInstallTarget,
		install:  service.Install,
	})
}

type initSystemInstallOps struct {
	elevated bool
	plan     func(service.Config) (service.InstallPlan, error)
	install  func(service.Config, io.Writer) error
}

func installInitSystemServiceWithOps(payload initServicePayload, output io.Writer, ops initSystemInstallOps) error {
	if !ops.elevated {
		return unsavedInitServiceError(fmt.Errorf("administrator privileges are required"))
	}
	if ops.install == nil {
		return unsavedInitServiceError(fmt.Errorf("native service installer is unavailable"))
	}
	if ops.plan == nil {
		return unsavedInitServiceError(fmt.Errorf("native service installation planner is unavailable"))
	}
	if strings.TrimSpace(payload.BinaryPath) == "" || strings.TrimSpace(payload.ConfigPath) == "" || strings.TrimSpace(payload.ServiceName) == "" {
		return unsavedInitServiceError(fmt.Errorf("incomplete system service installation payload"))
	}
	locale, ok := i18n.Match(payload.Locale)
	if !ok {
		return unsavedInitServiceError(fmt.Errorf("unsupported system service installation locale %q", payload.Locale))
	}
	payload.Locale = locale
	plan, err := ops.plan(service.Config{
		Translator:  i18n.New(payload.Locale),
		BinaryPath:  payload.BinaryPath,
		ConfigPath:  payload.ConfigPath,
		ServiceName: payload.ServiceName,
	})
	if err != nil {
		return unsavedInitServiceError(fmt.Errorf("validate system service installation payload: %w", err))
	}
	if payload.BinaryPath != plan.BinaryPath || payload.ConfigPath != plan.ConfigPath || payload.ServiceName != plan.ServiceName {
		return unsavedInitServiceError(fmt.Errorf(
			"system service installation payload does not match its validated plan (binary=%q config=%q service=%q)",
			plan.BinaryPath,
			plan.ConfigPath,
			plan.ServiceName,
		))
	}
	if exists, err := existingRegularOrMissing(payload.ConfigPath, "config"); err != nil {
		return unsavedInitServiceError(err)
	} else if exists && payload.Update.Auth.Enabled {
		if _, err := secureConfigPermissions(payload.ConfigPath); err != nil {
			return unsavedInitServiceError(fmt.Errorf("secure system config permissions: %w", err))
		}
	}

	save, err := saveManagedSetupConfig(payload.ConfigPath, appdirs.System, payload.Update)
	if err != nil {
		if managedSetupConfigCommitted(err) {
			return savedInitServiceError(err)
		}
		return unsavedInitServiceError(err)
	}
	cfg := service.Config{
		Translator:  i18n.New(payload.Locale),
		BinaryPath:  payload.BinaryPath,
		ConfigPath:  payload.ConfigPath,
		ServiceName: payload.ServiceName,
	}
	if err := ops.install(cfg, output); err != nil {
		if save.ConfigCreated {
			if rollbackErr := rollbackManagedConfig(payload.ConfigPath, save); rollbackErr == nil {
				return unsavedInitServiceError(err)
			} else {
				return savedInitServiceError(fmt.Errorf("%w; rollback new system config: %v", err, rollbackErr))
			}
		}
		return savedInitServiceError(err)
	}
	return nil
}
