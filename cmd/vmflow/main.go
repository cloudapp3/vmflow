package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/cloudapp3/vmflow/internal/statsstore"
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
  vmflow                                                                  Show status and next steps
  vmflow init          [-config path]                                    Create the first forwarding rule
  vmflow run           [-config path] [-control-port port]               Start forwarding in foreground
                                                                         Default config: config.yaml beside vmflow
  vmflow status        [-config path] [-json]                            Inspect config and daemon status
  vmflow ctl           [-token token] <health|rules|stats|metrics|precheck|reload> Query running vmflow
  vmflow tui           [-token token]                                  Terminal UI dashboard
  vmflow mcp           [-token token]                                  Read-only MCP server over stdio
  vmflow version       [-json]                                         Show version info
  vmflow update        [--check] [--version tag]                       Self-update vmflow binary
  vmflow service       (install|uninstall|status) [--config path]      Register as a native OS service
                       [--user name] [--log-file path] [--binary path] (systemd / launchd / Windows Service)
                       [--control-port port]
                       [--extra-arg=-future-flag=value]...
  vmflow uninstall     [--dry-run]                                    Uninstall vmflow: remove service, binary,
                                                                       owned config/cache, logs, and update cache

Aliases: ctl=c, tui=t, version=v, update=u, service=svc, uninstall=remove,rm
`

func main() {
	command, args := routeCLI(os.Args[1:])
	switch command {
	case "guide":
		runGuide(args)
	case "init":
		runInit(args)
	case "run":
		runForeground(args)
	case "foreground":
		fmt.Fprintln(os.Stderr, "warning: starting vmflow without the 'run' command is deprecated; use 'vmflow run [flags]'")
		runForeground(args)
	case "status":
		runStatus(args)
	case "help":
		fmt.Fprint(os.Stdout, usageText)
	case "ctl", "c":
		runCtl(args)
	case "tui", "t":
		runTUI(args)
	case "mcp":
		runMCP(args)
	case "version", "v":
		runVersion(args)
	case "update", "u":
		runUpdate(args)
	case "service", "svc":
		runService(args)
	case "uninstall", "remove", "rm":
		runUninstall(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(1)
	}
}

func routeCLI(args []string) (string, []string) {
	if len(args) == 0 {
		return "guide", nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		return "help", args[1:]
	case "init", "run", "status", "ctl", "c", "tui", "t", "mcp", "version", "v", "update", "u", "service", "svc", "uninstall", "remove", "rm":
		return args[0], args[1:]
	default:
		if strings.HasPrefix(args[0], "-") {
			return "foreground", args
		}
		return "unknown", args
	}
}

// ── foreground runtime ──────────────────────────────────────────────

type foregroundOptions struct {
	configPath  string
	controlPort int
	logFile     string
	serviceName string
}

func parseForegroundOptions(args []string, resolveDefaultConfig func() (string, error), output io.Writer) (foregroundOptions, error) {
	fs := flag.NewFlagSet("vmflow", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage:\n  vmflow run [flags]\n\nRuns vmflow in the foreground.\n\nOptions:")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "config file path (default: config.yaml beside vmflow)")
	controlPort := fs.Int("control-port", 0, "override the local management port (0 uses config)")
	logFile := fs.String("log-file", "", "write logs to this file instead of stdout (useful under a service manager)")
	serviceName := fs.String("service-name", service.DefaultServiceName, "[Windows SCM] registered service name")
	if err := fs.Parse(args); err != nil {
		return foregroundOptions{}, err
	}
	if extra := fs.Args(); len(extra) != 0 {
		return foregroundOptions{}, fmt.Errorf("unexpected argument(s): %v", extra)
	}
	if *controlPort < 0 || *controlPort > 65535 {
		return foregroundOptions{}, fmt.Errorf("control-port must be 0 (use config) or between 1 and 65535")
	}

	resolvedConfig := strings.TrimSpace(*configPath)
	if resolvedConfig == "" {
		var err error
		resolvedConfig, err = resolveDefaultConfig()
		if err != nil {
			return foregroundOptions{}, fmt.Errorf("resolve default config path (use -config to override): %w", err)
		}
	}
	return foregroundOptions{
		configPath:  resolvedConfig,
		controlPort: *controlPort,
		logFile:     *logFile,
		serviceName: *serviceName,
	}, nil
}

func defaultRuntimeConfigPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("determine executable path: %w", err)
	}
	return configPathBesideExecutable(executable)
}

func configPathBesideExecutable(executable string) (string, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", errors.New("empty executable path")
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("make executable path absolute: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve executable path %s: %w", absolute, err)
	}
	return filepath.Join(filepath.Dir(resolved), "config.yaml"), nil
}

func runForeground(args []string) {
	opts, err := parseForegroundOptions(args, defaultRuntimeConfigPath, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid runtime arguments: %v\n", err)
		os.Exit(2)
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	startupConfig := cfg
	if opts.controlPort != 0 {
		cfg.ControlPort = opts.controlPort
	}

	logger, err := newLogger(cfg.Log, opts.logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger failed: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)
	if cfg.UsedDeprecatedControlListenAddr {
		logger.Warn("control_listen_addr is deprecated; replace it with control_port", "component", "config", "control_port", cfg.ControlPort)
	}

	// On Windows, when launched by the Service Control Manager, hand off to a
	// native service runner instead of the foreground loop. No-op elsewhere.
	if maybeRunAsService(cfg, startupConfig, opts.configPath, logger, opts.serviceName) {
		return
	}

	// systemd and launchd both stop services with SIGTERM; honor it alongside SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var reporter func(runtimeReadyInfo)
	if shouldPrintRuntimeSummary(cfg, opts) {
		reporter = func(info runtimeReadyInfo) {
			printRuntimeReady(os.Stdout, info)
		}
	}
	if err := runForwardingWithReporter(ctx, cfg, startupConfig, opts.configPath, logger, reporter); err != nil {
		fmt.Fprintf(os.Stderr, "vmflow failed: %v\n", err)
		os.Exit(1)
	}
}

// runForwarding loads the forwarding engine, control API, and optional bot from
// an already-parsed config, then blocks until ctx is cancelled (SIGINT/SIGTERM,
// or an SCM stop) or the control server fails. It performs graceful shutdown
// before returning. Shared by the foreground daemon and the Windows SCM runner.
func runForwarding(ctx context.Context, cfg, startupConfig config.File, configPath string, logger *slog.Logger) error {
	return runForwardingWithReporter(ctx, cfg, startupConfig, configPath, logger, nil)
}

func runForwardingWithReporter(ctx context.Context, cfg, startupConfig config.File, configPath string, logger *slog.Logger, reporter func(runtimeReadyInfo)) error {
	return runForwardingWithReadyAndReporter(ctx, cfg, startupConfig, configPath, logger, nil, reporter)
}

// runForwardingWithReady reports initialization success only after rules are
// active and the control listener is bound. Initialization failures are sent to
// ready before the function returns, allowing the Windows SCM runner to avoid a
// transient false Running state.
func runForwardingWithReady(ctx context.Context, cfg, startupConfig config.File, configPath string, logger *slog.Logger, ready chan<- error) (runErr error) {
	return runForwardingWithReadyAndReporter(ctx, cfg, startupConfig, configPath, logger, ready, nil)
}

func runForwardingWithReadyAndReporter(ctx context.Context, cfg, startupConfig config.File, configPath string, logger *slog.Logger, ready chan<- error, reporter func(runtimeReadyInfo)) (runErr error) {
	readyReported := false
	reportReady := func(err error) {
		if readyReported {
			return
		}
		readyReported = true
		if ready != nil {
			ready <- err
		}
	}
	defer func() { reportReady(runErr) }()

	tlsCfg, err := controlapi.BuildServerTLSConfig(cfg.ControlTLS)
	if err != nil {
		return fmt.Errorf("control api tls: %w", err)
	}

	collector := engine.NewCollector()
	manager := engine.NewManagerWithOptions(collector, engine.ManagerOptions{UDPMaxSessions: cfg.UDPMaxSessions})

	var statsStore *statsstore.Store
	if cfg.Stats.Persist {
		statsPath := statsFilePath(cfg, configPath)
		samePath, err := statsstore.SameFilePath(statsPath, configPath)
		if err != nil {
			return fmt.Errorf("stats persistence path: %w", err)
		}
		if samePath {
			return fmt.Errorf("stats persistence path must differ from config path: %s", statsPath)
		}
		statsStore = statsstore.New(statsPath)
		if snapshots, err := statsStore.Load(); err != nil {
			logger.Warn("stats load failed; starting from zero", "component", "stats", "error", err)
		} else if restored, ignored := configuredStats(snapshots, cfg.Rules); len(restored) > 0 {
			collector.Restore(restored)
			logger.Info("restored traffic stats", "component", "stats", "rules", len(restored), "ignored_rules", ignored)
		} else if ignored > 0 {
			logger.Info("ignored traffic stats for unconfigured rules", "component", "stats", "ignored_rules", ignored)
		}
	}

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
	if statsStore != nil {
		if err := statsStore.Save(manager.SnapshotAll()); err != nil {
			manager.StopAll()
			return fmt.Errorf("initialize stats persistence: %w", err)
		}
	}

	runtime := &controlapi.Runtime{
		ConfigPath:    configPath,
		ServerVersion: strings.TrimSpace(version),
		Commit:        optionalField(commit, "none"),
		StartedAt:     time.Now(),
		Manager:       manager,
		Logger:        logger,
		Auth:          controlapi.NewAuthenticator(cfg.Auth),
		Metrics:       metricsCollector,
		StartupConfig: &startupConfig,
	}
	handler := controlapi.NewHandler(runtime)
	botMgr := bot.NewManager(manager, newBotControlFn(handler, logger), logger)
	runtime.Bot = botMgr
	server := &http.Server{
		Addr:              cfg.ControlListenAddress(),
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		manager.StopAll()
		return fmt.Errorf("listen on control address %s: %w", server.Addr, err)
	}
	scheme := "http"
	errCh := make(chan error, 1)
	if tlsCfg != nil {
		scheme = "https"
		go func() { errCh <- server.ServeTLS(listener, "", "") }()
	} else {
		go func() { errCh <- server.Serve(listener) }()
	}
	logger.Info("vmflow control server listening", "component", "daemon", "event", "control_listen", "addr", listener.Addr().String(), "scheme", scheme, "mtls", tlsCfg != nil && tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert)

	if err := botMgr.Apply(controlapi.BotSettings{Token: cfg.BotToken, ChatID: cfg.BotChat, ControlToken: cfg.BotControlToken}); err != nil {
		logger.Error("bot start failed", "component", "bot", "event", "init_failed", "error", err)
	}
	var statsFlush *statsFlusher
	if statsStore != nil {
		statsFlush = startStatsFlusher(ctx, statsStore, manager, statsFlushInterval(cfg, logger), logger)
	}
	if reporter != nil {
		reporter(runtimeReadyInfo{
			ConfigPath:      configPath,
			ControlAddress:  scheme + "://" + listener.Addr().String(),
			ConfiguredRules: len(cfg.Rules),
			EnabledRules:    countEnabledRules(cfg.Rules),
			ActiveRules:     len(manager.RunningRules()),
		})
	}
	reportReady(nil)

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

	if statsFlush != nil {
		statsFlush.Stop()
	}
	botMgr.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("control server shutdown failed", "component", "daemon", "error", err)
		_ = server.Close()
	}
	manager.StopAll()
	if statsStore != nil {
		if err := statsStore.Save(manager.SnapshotAll()); err != nil {
			logger.Warn("stats shutdown flush failed", "component", "stats", "error", err)
		}
	}
	logger.Info("vmflow stopped", "component", "daemon", "event", "stopped")
	return serverErr
}

// statsFilePath resolves the persistence path: explicit config value, systemd
// state directory, then stats.json beside the config file.
func statsFilePath(cfg config.File, configPath string) string {
	return statsstore.ResolvePath(configPath, cfg.Stats.Path, statsStateDirectory())
}

func statsStateDirectory() string {
	for _, path := range filepath.SplitList(strings.TrimSpace(os.Getenv("STATE_DIRECTORY"))) {
		if path = strings.TrimSpace(path); filepath.IsAbs(path) {
			return path
		}
	}
	return ""
}

func configuredStats(snapshots []engine.TrafficSnapshot, rules []engine.Rule) ([]engine.TrafficSnapshot, int) {
	configured := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if ruleID := strings.TrimSpace(rule.RuleID); ruleID != "" {
			configured[ruleID] = struct{}{}
		}
	}
	restored := make([]engine.TrafficSnapshot, 0, len(snapshots))
	ignored := 0
	for _, snapshot := range snapshots {
		if _, ok := configured[strings.TrimSpace(snapshot.RuleID)]; !ok {
			ignored++
			continue
		}
		restored = append(restored, snapshot)
	}
	return restored, ignored
}

// statsFlushInterval parses stats.flush_interval (default 60s, minimum 1s).
func statsFlushInterval(cfg config.File, logger *slog.Logger) time.Duration {
	if s := strings.TrimSpace(cfg.Stats.FlushInterval); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= time.Second {
			return d
		}
		if logger != nil {
			logger.Warn("invalid stats.flush_interval; using default 60s", "component", "stats", "value", s)
		}
	}
	return 60 * time.Second
}

type statsFlusher struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

func startStatsFlusher(parent context.Context, store *statsstore.Store, manager *engine.Manager, interval time.Duration, logger *slog.Logger) *statsFlusher {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		flushStats(ctx, store, manager, interval, logger)
	}()
	return &statsFlusher{cancel: cancel, done: done}
}

func (flusher *statsFlusher) Stop() {
	if flusher == nil {
		return
	}
	flusher.cancel()
	<-flusher.done
}

// flushStats periodically persists cumulative counters until ctx is cancelled.
func flushStats(ctx context.Context, store *statsstore.Store, manager *engine.Manager, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := store.Save(manager.SnapshotAll()); err != nil {
				if logger != nil {
					logger.Warn("stats flush failed", "component", "stats", "error", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// newBotControlFn builds an in-process control client. Requests still pass
// through the production handler and bearer-token authorization, while mTLS
// remains enforced for every connection accepted by the external listener.
func newBotControlFn(handler http.Handler, logger *slog.Logger) func(controlToken string) *controlapi.Client {
	return func(controlToken string) *controlapi.Client {
		if strings.TrimSpace(controlToken) == "" {
			return nil
		}
		client := controlapi.NewClient("http://vmflow.internal", controlToken)
		client.SetHTTPClient(&http.Client{
			Timeout:   10 * time.Second,
			Transport: inProcessRoundTripper{handler: handler},
		})
		if logger != nil {
			logger.Info("bot control client ready", "component", "bot", "transport", "in_process")
		}
		return client
	}
}

type inProcessRoundTripper struct {
	handler http.Handler
}

func (transport inProcessRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.handler == nil {
		return nil, errors.New("in-process control handler is unavailable")
	}
	if request == nil {
		return nil, errors.New("in-process control request is nil")
	}
	recorder := &inProcessResponseWriter{header: make(http.Header)}
	internalRequest := request.Clone(request.Context())
	internalRequest.RequestURI = request.URL.RequestURI()
	internalRequest.RemoteAddr = "127.0.0.1:0"
	transport.handler.ServeHTTP(recorder, internalRequest)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	body := recorder.body.Bytes()
	return &http.Response{
		StatusCode:    recorder.status,
		Status:        fmt.Sprintf("%d %s", recorder.status, http.StatusText(recorder.status)),
		Header:        recorder.header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

type inProcessResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (writer *inProcessResponseWriter) Header() http.Header {
	return writer.header
}

func (writer *inProcessResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *inProcessResponseWriter) Write(payload []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return writer.body.Write(payload)
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
	defaults := loadManagementDefaults(os.Stderr)
	addr := fs.String("addr", defaults.Address, "daemon management address")
	token := fs.String("token", defaults.Token, "daemon management token (or environment/client profile)")
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	fs.Parse(args)

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vmflow ctl [-token token] <health|rules|stats|metrics|precheck|reload>")
		os.Exit(1)
	}

	var method string
	var path string
	var reqBody string
	switch cmdArgs[0] {
	case "health":
		method = http.MethodGet
		path = "/v1/session"
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
	defaults := loadManagementDefaults(os.Stderr)
	addr := fs.String("addr", defaults.Address, "daemon management address")
	token := fs.String("token", defaults.Token, "daemon management token (or environment/client profile)")
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
	if !checkOnly && !updater.SelfUpdateSupported() {
		fmt.Fprintln(os.Stderr, "self-update is not supported on Windows; install the release ZIP manually")
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
	logFile := fs.String("log-file", "", "override daemon log destination (Windows default: C:\\ProgramData\\vmflow\\logs\\vmflow.log)")
	controlPort := fs.Int("control-port", 0, "override the daemon local management port")
	var extraArgs serviceArgList
	fs.Var(&extraArgs, "extra-arg", "append a future daemon flag as --extra-arg=-flag=value (repeatable; existing flags have dedicated options)")
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
	if *controlPort < 0 || *controlPort > 65535 {
		fmt.Fprintln(os.Stderr, "control-port must be 0 (use config) or between 1 and 65535")
		os.Exit(2)
	}
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %v\n", extra)
		os.Exit(1)
	}

	cfg := service.Config{
		BinaryPath:  *binaryPath,
		ConfigPath:  *configPath,
		User:        *user,
		LogFile:     *logFile,
		ControlPort: *controlPort,
		ExtraArgs:   []string(extraArgs),
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

type serviceArgList []string

func (args *serviceArgList) String() string {
	if args == nil {
		return ""
	}
	return strings.Join(*args, " ")
}

func (args *serviceArgList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("extra argument must not be empty")
	}
	*args = append(*args, value)
	return nil
}
