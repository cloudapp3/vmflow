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
	"github.com/cloudapp3/vmflow/internal/service"
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
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %v\n", args)
		os.Exit(1)
	}
	defaults := loadManagementDefaults(os.Stderr)
	configPath := defaults.ConfigPath
	if configPath == "" {
		var err error
		configPath, err = defaultRuntimeConfigPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve default config: %v\n", err)
			fmt.Fprint(os.Stdout, usageText)
			return
		}
	}
	report := inspectStatus(statusInspectOptions{
		ConfigPath:       configPath,
		Address:          defaults.Address,
		Token:            defaults.Token,
		TLS:              clientTLSOptionsFromEnv(),
		Timeout:          800 * time.Millisecond,
		UseConfigAddress: !defaults.ProfileLoaded && strings.TrimSpace(os.Getenv("VMFLOW_CONTROL_ADDR")) == "",
	})
	printGuide(os.Stdout, report)
}

func runStatus(args []string) {
	defaults := loadManagementDefaults(os.Stderr)
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:\n  vmflow status [flags]\n\nInspects the local configuration and running daemon.\n\nOptions:")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", defaults.ConfigPath, "config file path (default: client profile or config.yaml beside vmflow)")
	address := fs.String("addr", defaults.Address, "daemon management address")
	token := fs.String("token", defaults.Token, "daemon management token (or environment/client profile)")
	asJSON := fs.Bool("json", false, "output JSON")
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	fs.Parse(args)
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %v\n", extra)
		os.Exit(1)
	}
	if strings.TrimSpace(*configPath) == "" {
		resolved, err := defaultRuntimeConfigPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve default config: %v\n", err)
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
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
		return
	}
	printStatus(os.Stdout, report)
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
	fmt.Fprintf(w, "vmflow %s\n\n", displayVersion(report.Version))
	fmt.Fprintf(w, "Status: %s", report.Status)
	if report.StatusDetail != "" {
		fmt.Fprintf(w, " (%s)", report.StatusDetail)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Config: %s", report.ConfigPath)
	switch report.ConfigState {
	case "missing":
		fmt.Fprint(w, " (not created)")
	case "invalid":
		fmt.Fprintf(w, " (invalid: %s)", report.ConfigError)
	}
	fmt.Fprintln(w)
	if report.ConfigState == "loaded" {
		fmt.Fprintf(w, "Rules: %d configured, %d enabled\n", report.ConfiguredRules, report.EnabledRules)
	}
	fmt.Fprintln(w, "\nNext steps:")
	switch {
	case report.Status == "running":
		fmt.Fprintln(w, "  vmflow tui                   Open the dashboard")
		fmt.Fprintln(w, "  vmflow status                Inspect daemon details")
		fmt.Fprintln(w, "  vmflow service status        Inspect the native service")
	case report.Status == "unknown":
		fmt.Fprintln(w, "  vmflow status                Inspect the process using the management port")
		fmt.Fprintln(w, "  vmflow help                  Show all commands")
	case report.ConfigState != "loaded" || report.EnabledRules == 0:
		fmt.Fprintf(w, "  vmflow init -config %q   Create your first forwarding rule\n", report.ConfigPath)
		fmt.Fprintln(w, "  vmflow help                  Show all commands")
	default:
		fmt.Fprintf(w, "  vmflow run -config %q    Run in the foreground\n", report.ConfigPath)
		fmt.Fprintf(w, "  vmflow service install --config %q\n", report.ConfigPath)
		fmt.Fprintln(w, "  vmflow status                Inspect daemon details")
	}
}

func printStatus(w io.Writer, report statusReport) {
	fmt.Fprintf(w, "vmflow %s\n", displayVersion(report.Version))
	fmt.Fprintf(w, "Status: %s", report.Status)
	if report.StatusDetail != "" {
		fmt.Fprintf(w, " (%s)", report.StatusDetail)
	}
	fmt.Fprintln(w)
	if report.DaemonVersion != "" {
		fmt.Fprintf(w, "Daemon: %s\n", displayVersion(report.DaemonVersion))
	}
	fmt.Fprintf(w, "Config: %s [%s]\n", report.ConfigPath, report.ConfigState)
	if report.ConfigError != "" {
		fmt.Fprintf(w, "Config error: %s\n", report.ConfigError)
	}
	fmt.Fprintf(w, "Rules: %d configured, %d enabled\n", report.ConfiguredRules, report.EnabledRules)
	fmt.Fprintf(w, "Control: %s\n", report.ControlAddress)
	if report.ServiceState == "" {
		fmt.Fprintln(w, "Service: not inspected")
	} else if report.ServiceDetail == "" {
		fmt.Fprintf(w, "Service: %s\n", report.ServiceState)
	} else {
		fmt.Fprintf(w, "Service: %s (%s)\n", report.ServiceState, report.ServiceDetail)
	}
}

func displayVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func shouldPrintRuntimeSummary(cfg config.File, options foregroundOptions) bool {
	return strings.TrimSpace(options.logFile) == "" &&
		strings.EqualFold(strings.TrimSpace(cfg.Log.Format), config.DefaultLogFormat) &&
		isTerminalFile(os.Stdout)
}

func printRuntimeReady(w io.Writer, info runtimeReadyInfo) {
	fmt.Fprintln(w, "\nvmflow is running")
	fmt.Fprintf(w, "Config: %s\n", info.ConfigPath)
	fmt.Fprintf(w, "Forwarding: %d active / %d enabled / %d configured\n", info.ActiveRules, info.EnabledRules, info.ConfiguredRules)
	fmt.Fprintf(w, "Control: %s\n", info.ControlAddress)
	if info.ActiveRules == 0 {
		fmt.Fprintln(w, "No forwarding rules are active.")
		fmt.Fprintf(w, "Next: press Ctrl+C, then run `vmflow init -config %q`\n", info.ConfigPath)
	} else {
		fmt.Fprintln(w, "Dashboard: open another terminal and run `vmflow tui`")
		fmt.Fprintln(w, "Status: vmflow status")
	}
	fmt.Fprintln(w, "Stop: Ctrl+C")
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
