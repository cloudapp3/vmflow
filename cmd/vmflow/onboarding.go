package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
)

const (
	projectGitHubURL     = "https://github.com/cloudapp3/vmflow"
	communityTelegramURL = "https://t.me/VMPulse"
)

type statusReport struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	Status          string `json:"status"`
	StatusDetail    string `json:"status_detail,omitempty"`
	DaemonVersion   string `json:"daemon_version,omitempty"`
	ConfigPath      string `json:"config_path"`
	ConfigState     string `json:"config_state"`
	ConfigError     string `json:"config_error,omitempty"`
	ConfiguredRules int    `json:"configured_rules"`
	EnabledRules    int    `json:"enabled_rules"`
	ControlAddress  string `json:"control_address"`
	ServiceState    string `json:"service_state,omitempty"`
	ServiceDetail   string `json:"service_detail,omitempty"`
	BundledExample  bool   `json:"-"`
	ConfigIsDefault bool   `json:"-"`
}

type runtimeReadyInfo struct {
	ConfigPath      string
	ControlAddress  string
	ConfiguredRules int
	EnabledRules    int
	ActiveRules     int
}

type statusInspectOptions struct {
	ConfigPath       string
	Address          string
	Token            string
	TLS              controlapi.ClientTLSOptions
	Headers          http.Header
	Timeout          time.Duration
	UseConfigAddress bool
}

func runGuide(args []string) {
	runGuideWithTranslator(args, defaultTranslator())
}

func runGuideWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.unexpected_arguments", args))
		os.Exit(1)
	}
	defaults := loadManagementDefaultsWithTranslator(os.Stderr, translator)
	defaultConfigPath, defaultConfigErr := defaultRuntimeConfigPath()
	configPath := defaults.ConfigPath
	if configPath == "" {
		if defaultConfigErr != nil {
			fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.resolve_default_config", defaultConfigErr))
			fmt.Fprint(os.Stdout, usageTextFor(translator))
			return
		}
		configPath = defaultConfigPath
	}
	report := inspectStatus(statusInspectOptions{
		ConfigPath:       configPath,
		Address:          defaults.Address,
		Token:            defaults.Token,
		TLS:              clientTLSOptionsFromEnv(),
		Timeout:          800 * time.Millisecond,
		UseConfigAddress: !defaults.ProfileLoaded && strings.TrimSpace(os.Getenv("VMFLOW_CONTROL_ADDR")) == "",
	})
	report.ConfigIsDefault = defaultConfigErr == nil && sameFilesystemPath(configPath, defaultConfigPath)
	printGuideWithTranslator(os.Stdout, report, translator)
}

func runStatus(args []string) {
	runStatusWithTranslator(args, defaultTranslator())
}

func runStatusWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	defaults := loadManagementDefaultsWithTranslator(os.Stderr, translator)
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), translator.T("cli.status.usage"))
		fs.PrintDefaults()
	}
	configPath := fs.String("config", defaults.ConfigPath, translator.T("cli.status.flag.config"))
	address := fs.String("addr", defaults.Address, translator.T("cli.tui.flag.address"))
	token := fs.String("token", defaults.Token, translator.T("cli.tui.flag.token"))
	asJSON := fs.Bool("json", false, translator.T("cli.status.flag.json"))
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	localizeControlClientFlags(fs, translator)
	fs.Parse(args)
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.unexpected_arguments", extra))
		os.Exit(1)
	}
	if strings.TrimSpace(*configPath) == "" {
		resolved, err := defaultRuntimeConfigPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.resolve_default_config", err))
			os.Exit(1)
		}
		*configPath = resolved
	}
	addressExplicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "addr" {
			addressExplicit = true
		}
	})

	report := inspectStatus(statusInspectOptions{
		ConfigPath:       *configPath,
		Address:          *address,
		Token:            *token,
		TLS:              tlsFlags.Opts(),
		Headers:          headerFlags.HTTPHeader(),
		Timeout:          2 * time.Second,
		UseConfigAddress: !addressExplicit && !defaults.ProfileLoaded && strings.TrimSpace(os.Getenv("VMFLOW_CONTROL_ADDR")) == "",
	})
	serviceSummary, serviceErr := service.Inspect(service.Config{})
	report.ServiceState = serviceSummary.State
	report.ServiceDetail = serviceSummary.Detail
	if serviceErr != nil {
		report.ServiceState = "unknown"
		report.ServiceDetail = serviceErr.Error()
	}
	_ = writeStatusReport(os.Stdout, report, *asJSON, translator)
}

func writeStatusReport(w io.Writer, report statusReport, asJSON bool, translator *i18n.Translator) error {
	if asJSON {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	printStatusWithTranslator(w, report, translator)
	return nil
}

func inspectStatus(opts statusInspectOptions) statusReport {
	if opts.Timeout <= 0 {
		opts.Timeout = time.Second
	}
	report := statusReport{
		Name:           "vmflow",
		Version:        version,
		Status:         "not running",
		ConfigPath:     strings.TrimSpace(opts.ConfigPath),
		ConfigState:    "missing",
		ControlAddress: strings.TrimRight(strings.TrimSpace(opts.Address), "/"),
	}
	if absolute, err := filepath.Abs(report.ConfigPath); err == nil {
		report.ConfigPath = absolute
	}

	cfg, err := config.Load(report.ConfigPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			report.ConfigState = "invalid"
			report.ConfigError = err.Error()
		}
	} else {
		report.ConfigState = "loaded"
		report.ConfiguredRules = len(cfg.Rules)
		report.BundledExample = len(cfg.Rules) == 1 && isBundledExampleRule(cfg.Rules[0])
		for _, rule := range cfg.Rules {
			if rule.Enabled {
				report.EnabledRules++
			}
		}
		if opts.UseConfigAddress || report.ControlAddress == "" {
			report.ControlAddress = controlAddressForConfig(cfg)
		}
	}
	if report.ControlAddress == "" {
		report.ControlAddress = "http://" + config.DefaultControlListenAddr
	}

	probe := probeDaemon(report.ControlAddress, strings.TrimSpace(opts.Token), opts.TLS, opts.Headers, opts.Timeout)
	report.Status = probe.Status
	report.StatusDetail = probe.Detail
	report.DaemonVersion = probe.Version
	return report
}

type daemonProbe struct {
	Status  string
	Detail  string
	Version string
}

func probeDaemon(address, token string, tlsOpts controlapi.ClientTLSOptions, headers http.Header, timeout time.Duration) daemonProbe {
	client := controlapi.NewClient(address, token)
	httpClient, err := controlapi.NewHTTPClient(tlsOpts, timeout)
	if err != nil {
		return daemonProbe{Status: "unknown", Detail: "management TLS configuration is invalid: " + err.Error()}
	}
	client.SetHTTPClient(httpClient)
	client.SetHeaders(headers)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	session, err := client.Session(ctx)
	if err == nil {
		return daemonProbe{Status: "running", Version: strings.TrimSpace(session.ServerVersion)}
	}
	if status := controlapi.APIStatus(err); status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return daemonProbe{Status: "running", Detail: "management authentication is required"}
	}

	open, dialErr := managementPortOpen(address, timeout)
	if !open {
		if dialErr != nil {
			return daemonProbe{Status: "not running"}
		}
		return daemonProbe{Status: "not running"}
	}
	return daemonProbe{Status: "unknown", Detail: "management port is open but did not identify a vmflow daemon: " + err.Error()}
}

func managementPortOpen(address string, timeout time.Duration) (bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return false, err
	}
	host := parsed.Host
	if parsed.Port() == "" {
		switch parsed.Scheme {
		case "http":
			host = net.JoinHostPort(parsed.Hostname(), "80")
		case "https":
			host = net.JoinHostPort(parsed.Hostname(), "443")
		default:
			return false, fmt.Errorf("unsupported management scheme")
		}
	}
	connection, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return false, err
	}
	_ = connection.Close()
	return true, nil
}

func controlAddressForConfig(cfg config.File) string {
	scheme := "http"
	if strings.TrimSpace(cfg.ControlTLS.CertFile) != "" && strings.TrimSpace(cfg.ControlTLS.KeyFile) != "" {
		scheme = "https"
	}
	return scheme + "://" + cfg.ControlListenAddress()
}

func clientTLSOptionsFromEnv() controlapi.ClientTLSOptions {
	insecure := strings.EqualFold(strings.TrimSpace(os.Getenv("VMFLOW_TLS_INSECURE")), "true") || strings.TrimSpace(os.Getenv("VMFLOW_TLS_INSECURE")) == "1"
	return controlapi.ClientTLSOptions{
		CAFile:             strings.TrimSpace(os.Getenv("VMFLOW_TLS_CA_FILE")),
		ClientCertFile:     strings.TrimSpace(os.Getenv("VMFLOW_TLS_CLIENT_CERT")),
		ClientKeyFile:      strings.TrimSpace(os.Getenv("VMFLOW_TLS_CLIENT_KEY")),
		InsecureSkipVerify: insecure,
	}
}

func printGuide(w io.Writer, report statusReport) {
	printGuideWithTranslator(w, report, defaultTranslator())
}

func printGuideWithTranslator(w io.Writer, report statusReport, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintf(w, "vmflow %s\n\n", displayVersionWithTranslator(report.Version, translator))

	switch {
	case report.Status == "running":
		fmt.Fprintln(w, translator.T("cli.guide.running"))
		if report.ConfigState == "invalid" {
			fmt.Fprintln(w, translator.T("cli.guide.running_invalid_config"))
			fmt.Fprintln(w, translator.Tf("cli.output.config", report.ConfigPath))
			fmt.Fprintln(w, translator.Tf("cli.output.problem", report.ConfigError))
		} else {
			printGuideForwardingStateWithTranslator(w, report, translator)
			fmt.Fprintln(w, translator.Tf("cli.output.config", report.ConfigPath))
		}
		fmt.Fprintln(w, translator.T("cli.guide.open_dashboard"))
		fmt.Fprintln(w, "  vmflow tui")
		fmt.Fprintln(w, translator.T("cli.guide.other_commands"))
		fmt.Fprintln(w, translator.T("cli.guide.command.status"))
		fmt.Fprintln(w, translator.T("cli.guide.command.service_status"))
	case report.ConfigState == "invalid":
		fmt.Fprintln(w, translator.T("cli.guide.invalid_config"))
		fmt.Fprintln(w, translator.Tf("cli.output.config", report.ConfigPath))
		fmt.Fprintln(w, translator.Tf("cli.output.problem", report.ConfigError))
		fmt.Fprintln(w, translator.T("cli.guide.fix_config"))
	case report.Status == "unknown":
		fmt.Fprintln(w, translator.T("cli.guide.unverified_port"))
		if report.StatusDetail != "" {
			fmt.Fprintln(w, translator.Tf("cli.output.detail", report.StatusDetail))
		}
		fmt.Fprintln(w, translator.Tf("cli.output.config", report.ConfigPath))
		fmt.Fprintln(w, translator.T("cli.guide.inspect_status"))
	case report.ConfigState != "loaded" || report.EnabledRules == 0:
		fmt.Fprintln(w, translator.T("cli.guide.not_running"))
		printGuideForwardingStateWithTranslator(w, report, translator)
		if report.ConfigState == "missing" {
			fmt.Fprintln(w, translator.Tf("cli.output.config_will_create", report.ConfigPath))
		} else {
			fmt.Fprintln(w, translator.Tf("cli.output.config", report.ConfigPath))
		}
		fmt.Fprintln(w, translator.T("cli.guide.first_time_setup"))
		fmt.Fprintf(w, "  1. %s\n", guideConfigCommand("vmflow init", report))
		fmt.Fprintln(w, translator.T("cli.guide.first_time_rule"))
		fmt.Fprintln(w, translator.T("cli.guide.first_time_start"))
		fmt.Fprintln(w, translator.T("cli.guide.first_time_tui"))
		if report.BundledExample {
			fmt.Fprintln(w, translator.T("cli.guide.example_safe"))
		}
	default:
		fmt.Fprintln(w, translator.T("cli.guide.ready"))
		printGuideForwardingStateWithTranslator(w, report, translator)
		fmt.Fprintln(w, translator.Tf("cli.output.config", report.ConfigPath))
		fmt.Fprintln(w, translator.T("cli.guide.start"))
		fmt.Fprintln(w, translator.Tf("cli.guide.start_foreground", guideConfigCommand("vmflow run", report)))
		fmt.Fprintln(w, translator.Tf("cli.guide.start_boot", guideServiceInstallCommand(report)))
		fmt.Fprintln(w, translator.T("cli.guide.after_start"))
	}

	printGuideResourcesWithTranslator(w, translator)
}

func printGuideResources(w io.Writer) {
	printGuideResourcesWithTranslator(w, defaultTranslator())
}

func printGuideResourcesWithTranslator(w io.Writer, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(w, translator.T("cli.guide.more"))
	fmt.Fprintln(w, translator.T("cli.guide.commands"))
	fmt.Fprintf(w, "  GitHub:    %s\n", projectGitHubURL)
	fmt.Fprintf(w, "  Telegram:  %s\n", communityTelegramURL)
}

func printGuideForwardingState(w io.Writer, report statusReport) {
	printGuideForwardingStateWithTranslator(w, report, defaultTranslator())
}

func printGuideForwardingStateWithTranslator(w io.Writer, report statusReport, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	switch {
	case report.BundledExample:
		fmt.Fprintln(w, translator.T("cli.guide.forwarding_example"))
	case report.ConfigState != "loaded" || report.ConfiguredRules == 0:
		fmt.Fprintln(w, translator.T("cli.guide.forwarding_none"))
	default:
		fmt.Fprintln(w, translator.Tf("cli.guide.forwarding_counts", report.EnabledRules, report.ConfiguredRules))
	}
}

func guideConfigCommand(command string, report statusReport) string {
	if report.ConfigIsDefault || strings.TrimSpace(report.ConfigPath) == "" {
		return command
	}
	return fmt.Sprintf("%s -config %q", command, report.ConfigPath)
}

func guideServiceInstallCommand(report statusReport) string {
	if report.ConfigIsDefault || strings.TrimSpace(report.ConfigPath) == "" {
		return "vmflow service install"
	}
	return fmt.Sprintf("vmflow service install --config %q", report.ConfigPath)
}

func printStatus(w io.Writer, report statusReport) {
	printStatusWithTranslator(w, report, defaultTranslator())
}

func printStatusWithTranslator(w io.Writer, report statusReport, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintf(w, "vmflow %s\n", displayVersionWithTranslator(report.Version, translator))
	fmt.Fprintf(w, "%s: %s", translator.T("cli.status.label.status"), localizedState(translator, report.Status))
	if report.StatusDetail != "" {
		fmt.Fprintf(w, " (%s)", report.StatusDetail)
	}
	fmt.Fprintln(w)
	if report.DaemonVersion != "" {
		fmt.Fprintf(w, "%s: %s\n", translator.T("cli.status.label.daemon"), displayVersionWithTranslator(report.DaemonVersion, translator))
	}
	fmt.Fprintf(w, "%s: %s [%s]\n", translator.T("cli.label.config"), report.ConfigPath, localizedState(translator, report.ConfigState))
	if report.ConfigError != "" {
		fmt.Fprintf(w, "%s: %s\n", translator.T("cli.status.label.config_error"), report.ConfigError)
	}
	fmt.Fprintln(w, translator.Tf("cli.status.rules", report.ConfiguredRules, report.EnabledRules))
	fmt.Fprintf(w, "%s: %s\n", translator.T("cli.status.label.control"), report.ControlAddress)
	if report.ServiceState == "" {
		fmt.Fprintln(w, translator.T("cli.status.service_not_inspected"))
	} else if report.ServiceDetail == "" {
		fmt.Fprintf(w, "%s: %s\n", translator.T("cli.status.label.service"), localizedState(translator, report.ServiceState))
	} else {
		fmt.Fprintf(w, "%s: %s (%s)\n", translator.T("cli.status.label.service"), localizedState(translator, report.ServiceState), report.ServiceDetail)
	}
}

func displayVersion(value string) string {
	return displayVersionWithTranslator(value, defaultTranslator())
}

func displayVersionWithTranslator(value string, translator *i18n.Translator) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return translatorOrEnglish(translator).T("common.unknown")
	}
	return value
}

func localizedState(translator *i18n.Translator, value string) string {
	translator = translatorOrEnglish(translator)
	key := ""
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "running":
		key = "state.running"
	case "not running":
		key = "state.not_running"
	case "unknown":
		key = "common.unknown"
	case "loaded":
		key = "state.loaded"
	case "missing":
		key = "state.missing"
	case "invalid":
		key = "state.invalid"
	case "installed":
		key = "state.installed"
	case "not installed":
		key = "state.not_installed"
	}
	if key == "" {
		return value
	}
	return translator.T(key)
}

func shouldPrintRuntimeSummary(cfg config.File, options foregroundOptions) bool {
	return strings.TrimSpace(options.logFile) == "" &&
		strings.EqualFold(strings.TrimSpace(cfg.Log.Format), config.DefaultLogFormat) &&
		isTerminalFile(os.Stdout)
}

func printRuntimeReady(w io.Writer, info runtimeReadyInfo) {
	printRuntimeReadyWithTranslator(w, info, defaultTranslator())
}

func printRuntimeReadyWithTranslator(w io.Writer, info runtimeReadyInfo, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(w, translator.T("cli.runtime.running"))
	fmt.Fprintln(w, translator.Tf("cli.output.config", info.ConfigPath))
	fmt.Fprintln(w, translator.Tf("cli.runtime.forwarding", info.ActiveRules, info.EnabledRules, info.ConfiguredRules))
	fmt.Fprintf(w, "%s: %s\n", translator.T("cli.status.label.control"), info.ControlAddress)
	if info.ActiveRules == 0 {
		fmt.Fprintln(w, translator.T("cli.runtime.no_active_rules"))
		fmt.Fprintln(w, translator.Tf("cli.runtime.next_init", info.ConfigPath))
	} else {
		fmt.Fprintln(w, translator.T("cli.runtime.dashboard"))
		fmt.Fprintln(w, translator.T("cli.runtime.status"))
		fmt.Fprintln(w, translator.Tf("cli.runtime.background", nativeServiceInstallCommand(info.ConfigPath)))
	}
	fmt.Fprintln(w, translator.T("cli.runtime.stop"))
}

func countEnabledRules(rules []engine.Rule) int {
	count := 0
	for _, rule := range rules {
		if rule.Enabled {
			count++
		}
	}
	return count
}
