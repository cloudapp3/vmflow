package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudapp3/vmflow/bot"
	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/logging"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/internal/updater"
	"github.com/cloudapp3/vmflow/metrics"
	"github.com/cloudapp3/vmflow/tui"
)

// Build metadata injected via -ldflags.
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

const usageText = `vmflow - L4 port forwarding engine

Usage:
  vmflow daemon        [-config path] [-control-listen addr]              Start forwarding daemon
                       [-insecure-allow-remote-control]                  Allow non-loopback control API without auth (dangerous)
  vmflow ctl           [-addr url] [-token token] <health|rules|stats|metrics|precheck|reload>    Query running daemon
  vmflow tui           [-addr url] [-token token]                      Terminal UI dashboard
  vmflow version       [-json]                                         Show version info
  vmflow update        [--check] [--version tag]                       Self-update vmflow binary
  vmflow service       (install|uninstall|status) [--config path]      Register as a native OS service
                       [--user name] [--log-file path] [--binary path] (systemd / launchd / Windows Service)

Aliases: daemon=d, ctl=c, tui=t, version=v, update=u, service=svc
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	switch args[0] {
	case "daemon", "d":
		runDaemon(args[1:])
	case "ctl", "c":
		runCtl(args[1:])
	case "tui", "t":
		runTUI(args[1:])
	case "version", "v":
		runVersion(args[1:])
	case "update", "u":
		runUpdate(args[1:])
	case "service", "svc":
		runService(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		flag.Usage()
		os.Exit(1)
	}
}

// ── daemon ──────────────────────────────────────────────────────────

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")
	controlListen := fs.String("control-listen", "", "override control listen addr")
	insecureAllowRemote := fs.Bool("insecure-allow-remote-control", false,
		"DANGEROUS: allow binding the control API on a non-loopback address without auth")
	logFile := fs.String("log-file", "", "write logs to this file instead of stdout (useful under a service manager)")
	fs.Parse(args)

	if strings.TrimSpace(*configPath) == "" {
		fmt.Fprintln(os.Stderr, "missing -config")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(*controlListen) != "" {
		cfg.ControlListenAddr = strings.TrimSpace(*controlListen)
	}

	logger, err := newLogger(cfg.Log, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger failed: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	// On Windows, when launched by the Service Control Manager, hand off to a
	// native service runner instead of the foreground loop. No-op elsewhere.
	if maybeRunAsService(cfg, *configPath, logger, *insecureAllowRemote) {
		return
	}

	// systemd and launchd both stop services with SIGTERM; honor it alongside SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runForwarding(ctx, cfg, *configPath, logger, *insecureAllowRemote); err != nil {
		os.Exit(1)
	}
}

// runForwarding loads the forwarding engine, control API, and optional bot from
// an already-parsed config, then blocks until ctx is cancelled (SIGINT/SIGTERM,
// or an SCM stop) or the control server fails. It performs graceful shutdown
// before returning. Shared by the foreground daemon and the Windows SCM runner.
func runForwarding(ctx context.Context, cfg config.File, configPath string, logger *slog.Logger, insecureAllowRemote bool) error {
	if err := controlapi.EnsureSafeControlBinding(cfg, insecureAllowRemote, logger); err != nil {
		return fmt.Errorf("control api: %w", err)
	}

	tlsCfg, err := controlapi.BuildServerTLSConfig(cfg.ControlTLS)
	if err != nil {
		return fmt.Errorf("control api tls: %w", err)
	}

	collector := engine.NewCollector()
	manager := engine.NewManager(collector)

	metricsCollector := metrics.New(manager)
	result := manager.ApplySnapshot(cfg.Rules, engine.ApplySnapshotOptions{ReplaceAll: true})
	metricsCollector.ObserveApplyResult(result)
	if result.FailedRules > 0 {
		payload, _ := json.MarshalIndent(result, "", "  ")
		logger.Error("initial apply failed", "component", "engine", "event", "initial_apply_failed", "result", string(payload))
		manager.StopAll()
		return fmt.Errorf("initial apply failed: %d rule(s)", result.FailedRules)
	}
	logger.Info("initial snapshot applied", "component", "engine", "event", "initial_apply", "rule_count", len(cfg.Rules), "applied_rules", result.AppliedRules, "stopped_rules", result.StoppedRules)

	runtime := &controlapi.Runtime{
		ConfigPath: configPath,
		Manager:    manager,
		Logger:     logger,
		Auth:       controlapi.NewAuthenticator(cfg.Auth),
		Metrics:    metricsCollector,
	}
	server := &http.Server{
		Addr:              cfg.ControlListenAddr,
		Handler:           controlapi.NewHandler(runtime),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		scheme, listen := "http", server.ListenAndServe
		if tlsCfg != nil {
			scheme = "https"
			listen = func() error { return server.ListenAndServeTLS("", "") }
		}
		logger.Info("vmflow control server listening", "component", "daemon", "event", "control_listen", "addr", cfg.ControlListenAddr, "scheme", scheme, "mtls", tlsCfg != nil && tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert)
		errCh <- listen()
	}()

	var botCancel context.CancelFunc
	if cfg.BotToken != "" && cfg.BotChat != 0 {
		tgBot, err := bot.NewBot(cfg.BotToken, cfg.BotChat, manager)
		if err != nil {
			logger.Error("bot init failed", "component", "bot", "event", "init_failed", "error", err)
		} else {
			botCtx, cancel := context.WithCancel(context.Background())
			botCancel = cancel
			go func() {
				if err := tgBot.Start(botCtx); err != nil {
					logger.Warn("bot stopped", "component", "bot", "event", "stopped", "error", err)
				}
			}()
			logger.Info("telegram bot started", "component", "bot", "event", "started")
		}
	}

	// Block until shutdown is requested (ctx) or the server exits on its own.
	var serverErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received", "component", "daemon", "event", "shutdown_signal")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr = err
			logger.Error("server failed", "component", "daemon", "event", "server_failed", "error", err)
		}
	}

	if botCancel != nil {
		botCancel()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	manager.StopAll()
	logger.Info("vmflow stopped", "component", "daemon", "event", "stopped")
	return serverErr
}

// newLogger builds the structured logger. When logFile is empty it logs to
// stdout (captured by journald on Linux or launchd's StandardOutPath on macOS);
// otherwise it appends to the file, which is required under the Windows SCM
// where the process has no stdout.
func newLogger(logCfg config.LogConfig, logFile string) (*slog.Logger, error) {
	logFile = strings.TrimSpace(logFile)
	if logFile == "" {
		return logging.New(logCfg)
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return logging.NewWithWriter(logCfg, f)
}

// ── ctl ─────────────────────────────────────────────────────────────

func runCtl(args []string) {
	fs := flag.NewFlagSet("ctl", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:19090", "control api base url")
	token := fs.String("token", os.Getenv("VMFLOW_CONTROL_TOKEN"), "control api bearer token (or VMFLOW_CONTROL_TOKEN)")
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	fs.Parse(args)

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vmflow ctl [-addr url] [-token token] <health|rules|stats|metrics|precheck|reload>")
		os.Exit(1)
	}

	var method string
	var path string
	var reqBody string
	switch cmdArgs[0] {
	case "health":
		method = http.MethodGet
		path = "/healthz"
	case "rules":
		method = http.MethodGet
		path = "/v1/rules"
	case "stats":
		method = http.MethodGet
		path = "/v1/stats"
	case "metrics":
		method = http.MethodGet
		path = "/metrics"
	case "precheck":
		method = http.MethodPost
		path = "/v1/precheck"
	case "reload":
		method = http.MethodPost
		path = "/v1/reload"
	default:
		fmt.Fprintf(os.Stderr, "unknown action: %s\n", cmdArgs[0])
		os.Exit(1)
	}

	status, body, err := doRequest(*addr, *token, tlsFlags.Opts(), *headerFlags, method, path, reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if status >= 400 {
		fmt.Fprint(os.Stderr, string(body))
		os.Exit(1)
	}
	fmt.Print(string(body))
}

func doRequest(baseURL, token string, tlsOpts controlapi.ClientTLSOptions, headers controlapi.HeaderFlags, method, path, body string) (int, []byte, error) {
	hc, err := controlapi.NewHTTPClient(tlsOpts, 0)
	if err != nil {
		return 0, nil, err
	}
	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	headers.Apply(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// ── tui ─────────────────────────────────────────────────────────────

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:19090", "relayd control API address")
	token := fs.String("token", os.Getenv("VMFLOW_CONTROL_TOKEN"), "control api bearer token (or VMFLOW_CONTROL_TOKEN)")
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var httpClient *http.Client
	if tlsFlags.Opts().Any() {
		hc, err := controlapi.NewHTTPClient(tlsFlags.Opts(), 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tls: %v\n", err)
			os.Exit(1)
		}
		httpClient = hc
	}

	if err := tui.Run(ctx, os.Stdout, *addr, *token, httpClient, headerFlags.HTTPHeader()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ── version ─────────────────────────────────────────────────────────

func runVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "output as JSON")
	fs.Parse(args)

	if asJSON {
		type versionInfo struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			Commit    string `json:"commit,omitempty"`
			BuildTime string `json:"build_time,omitempty"`
		}
		info := versionInfo{
			Name:      "vmflow",
			Version:   version,
			Commit:    optionalField(commit, "none"),
			BuildTime: optionalField(buildTime, "unknown"),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(info)
		return
	}

	fmt.Printf("vmflow %s\n", version)
	if commit != "none" && commit != "" {
		fmt.Printf("commit:     %s\n", commit)
	}
	if buildTime != "unknown" && buildTime != "" {
		fmt.Printf("built:      %s\n", buildTime)
	}
}

func optionalField(value string, markers ...string) string {
	value = strings.TrimSpace(value)
	for _, marker := range markers {
		if value == marker {
			return ""
		}
	}
	return value
}

// ── update ──────────────────────────────────────────────────────────

func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  vmflow update [--check] [--version <tag>]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	var checkOnly bool
	var targetVersion string
	fs.BoolVar(&checkOnly, "check", false, "check for updates without installing")
	fs.StringVar(&targetVersion, "version", "", "install or inspect a specific release tag")
	fs.Parse(args)
	if len(fs.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "update does not accept positional args")
		os.Exit(1)
	}

	currentRaw := strings.TrimSpace(version)
	if !checkOnly && targetVersion == "" && strings.EqualFold(currentRaw, "dev") {
		fmt.Fprintf(os.Stderr, "self-update requires a tagged release build; current version is %q (use --version vX.Y.Z to install a specific release)\n", version)
		os.Exit(1)
	}

	client := updater.New(updater.Config{
		Repo:        "cloudapp3/vmflow",
		BinaryName:  "vmflow",
		CurrentVer:  version,
		GitHubToken: updateTokenFromEnv(),
		CacheDir:    updater.CacheDir(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	targetTag := normalizeReleaseTag(targetVersion)
	var (
		result *updater.CheckResult
		err    error
	)
	if targetTag != "" {
		result, err = client.CheckSpecificVersion(ctx, targetTag)
	} else {
		result, err = client.CheckForUpdate(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to check for updates: %v\n", err)
		os.Exit(1)
	}
	if result == nil {
		fmt.Fprintln(os.Stderr, "failed to check for updates: empty result")
		os.Exit(1)
	}

	if checkOnly {
		writeUpdateCheck(os.Stdout, result, targetTag != "")
		return
	}

	if !result.UpdateAvailable {
		if targetTag != "" && normalizeReleaseTag(result.CurrentVersion) == normalizeReleaseTag(result.LatestVersion) {
			fmt.Printf("already on requested version: %s\n", formatReleaseTag(result.LatestVersion))
			return
		}
		if targetTag != "" {
			fmt.Printf("requested version %s is not newer than current %s\n", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion))
			return
		}
		fmt.Printf("already up to date: %s\n", formatReleaseTag(result.CurrentVersion))
		return
	}

	if result.Release == nil {
		fmt.Fprintln(os.Stderr, "failed to install update: release metadata is unavailable")
		os.Exit(1)
	}

	fmt.Printf("updating from %s to %s\n", formatReleaseTag(result.CurrentVersion), formatReleaseTag(result.LatestVersion))
	if err := client.DownloadAndInstall(ctx, result.Release, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "failed to install update: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("updated successfully to %s\n", formatReleaseTag(result.LatestVersion))
}

func writeUpdateCheck(w io.Writer, result *updater.CheckResult, specific bool) {
	switch {
	case result.UpdateAvailable && specific:
		fmt.Fprintf(w, "target release available: %s (current %s)\n", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion))
	case result.UpdateAvailable:
		fmt.Fprintf(w, "update available: %s (current %s)\n", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion))
	case specific && normalizeReleaseTag(result.CurrentVersion) == normalizeReleaseTag(result.LatestVersion):
		fmt.Fprintf(w, "already on requested version: %s\n", formatReleaseTag(result.LatestVersion))
	case specific:
		fmt.Fprintf(w, "requested version %s is not newer than current %s\n", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion))
	default:
		fmt.Fprintf(w, "already up to date: %s\n", formatReleaseTag(result.CurrentVersion))
	}
}

func updateTokenFromEnv() string {
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GH_TOKEN"))
}

func normalizeReleaseTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "dev") {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

func formatReleaseTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	if strings.EqualFold(v, "dev") {
		return v
	}
	return normalizeReleaseTag(v)
}

// ── service ─────────────────────────────────────────────────────────

func runService(args []string) {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  vmflow service (install|uninstall|status) [flags]\n\n")
		fmt.Fprintf(fs.Output(), "Registers vmflow as a native service that starts at boot and restarts on crash:\n")
		fmt.Fprintf(fs.Output(), "  Linux:   systemd unit (/etc/systemd/system/<name>.service)\n")
		fmt.Fprintf(fs.Output(), "  macOS:   launchd daemon (/Library/LaunchDaemons/io.cloudapp.<name>.plist)\n")
		fmt.Fprintf(fs.Output(), "  Windows: Windows Service (manage via services.msc)\n\nOptions:\n")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "config file path the service runs with (default: platform system path)")
	user := fs.String("user", "", "[systemd] run as this user; created if missing (default: root)")
	logFile := fs.String("log-file", "", "redirect daemon logs here (required on Windows)")
	extraArgs := fs.String("extra-args", "", "extra flags appended to the daemon command line, e.g. \"-control-listen 0.0.0.0:19090\"")
	binaryPath := fs.String("binary", "", "path to the vmflow binary (default: this executable; install requires a trusted root/admin-owned absolute path)")

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vmflow service (install|uninstall|status) [flags]")
		os.Exit(1)
	}
	// The action is the first positional; flags follow it. The flag package
	// stops parsing at the first non-flag, so peel the action off before Parse.
	action := args[0]
	if strings.HasPrefix(action, "-") {
		fmt.Fprintln(os.Stderr, "usage: vmflow service (install|uninstall|status) [flags]")
		os.Exit(1)
	}
	fs.Parse(args[1:])
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %v\n", extra)
		os.Exit(1)
	}

	cfg := service.Config{
		BinaryPath: *binaryPath,
		ConfigPath: *configPath,
		User:       *user,
		LogFile:    *logFile,
		ExtraArgs:  *extraArgs,
	}

	var err error
	switch action {
	case "install":
		err = service.Install(cfg, os.Stdout)
	case "uninstall":
		err = service.Uninstall(cfg, os.Stdout)
	case "status":
		err = service.Status(cfg, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown service action: %s (expected install|uninstall|status)\n", action)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "service %s failed: %v\n", action, err)
		os.Exit(1)
	}
}
