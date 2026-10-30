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
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/logging"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/internal/statsstore"
	"github.com/cloudapp3/vmflow/internal/updater"
	"github.com/cloudapp3/vmflow/metrics"
	"github.com/cloudapp3/vmflow/netinfo"
	"github.com/cloudapp3/vmflow/tui"
)

// Build metadata injected via -ldflags.
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	environmentTranslator := i18n.New(i18n.Detect("", "", os.Getenv))
	explicitLanguage, cliArgs, err := extractLanguageArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", environmentTranslator.Tf("cli.error.invalid_language_arguments", err))
		os.Exit(2)
	}
	if explicitLanguage != "" {
		if _, ok := i18n.Match(explicitLanguage); !ok {
			fmt.Fprintf(os.Stderr, "%s\n", environmentTranslator.Tf("cli.error.unsupported_language", explicitLanguage, strings.Join(i18n.SupportedLocales(), ", ")))
			os.Exit(2)
		}
	}
	command, args := routeCLI(cliArgs)
	configured := loadConfiguredLanguage(command, args)
	translator := i18n.New(i18n.Detect(explicitLanguage, configured.Locale, os.Getenv))
	printInvalidConfiguredLanguage(os.Stderr, configured, translator)
	switch command {
	case "guide":
		runGuideWithTranslator(args, translator)
	case "init":
		runInitWithTranslator(args, translator)
	case "run":
		runForegroundWithTranslator(args, translator)
	case "foreground":
		fmt.Fprintln(os.Stderr, translator.T("cli.warning.legacy_foreground"))
		runForegroundWithTranslator(args, translator)
	case "status":
		runStatusWithTranslator(args, translator)
	case "help":
		fmt.Fprint(os.Stdout, usageTextFor(translator))
	case "ctl", "c":
		runCtlWithTranslator(args, translator)
	case "tui", "t":
		runTUIWithTranslator(args, translator)
	case "mcp":
		runMCPWithTranslator(args, translator)
	case "version", "v":
		runVersionWithTranslator(args, translator)
	case "update", "u":
		runUpdateWithTranslator(args, translator)
	case "service", "svc":
		runServiceWithTranslator(args, translator)
	case initServiceHelperCommand:
		runInitServiceHelper(args, translator)
	case "uninstall", "remove", "rm":
		runUninstallWithTranslator(args, translator)
	default:
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.unknown_command", args[0]))
		fmt.Fprint(os.Stderr, usageTextFor(translator))
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
	case "init", "run", "status", "ctl", "c", "tui", "t", "mcp", "version", "v", "update", "u", "service", "svc", "uninstall", "remove", "rm", initServiceHelperCommand:
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
	return parseForegroundOptionsWithTranslator(args, resolveDefaultConfig, output, defaultTranslator())
}

func parseForegroundOptionsWithTranslator(args []string, resolveDefaultConfig func() (string, error), output io.Writer, translator *i18n.Translator) (foregroundOptions, error) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("vmflow", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, translator.T("cli.run.usage"))
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", translator.T("cli.flag.config"))
	controlPort := fs.Int("control-port", 0, translator.T("cli.run.flag.control_port"))
	logFile := fs.String("log-file", "", translator.T("cli.run.flag.log_file"))
	serviceName := fs.String("service-name", service.DefaultServiceName, translator.T("cli.run.flag.service_name"))
	if err := fs.Parse(args); err != nil {
		return foregroundOptions{}, err
	}
	if extra := fs.Args(); len(extra) != 0 {
		return foregroundOptions{}, fmt.Errorf("%s", translator.Tf("cli.error.unexpected_arguments", extra))
	}
	if *controlPort < 0 || *controlPort > 65535 {
		return foregroundOptions{}, fmt.Errorf("%s", translator.T("cli.run.error.control_port"))
	}

	resolvedConfig := strings.TrimSpace(*configPath)
	if resolvedConfig == "" {
		var err error
		resolvedConfig, err = resolveDefaultConfig()
		if err != nil {
			return foregroundOptions{}, fmt.Errorf("%s", translator.Tf("cli.run.error.resolve_default_config", err))
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
	return appdirs.UserConfigPath()
}

func runForeground(args []string) {
	runForegroundWithTranslator(args, defaultTranslator())
}

func runForegroundWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	opts, err := parseForegroundOptionsWithTranslator(args, defaultRuntimeConfigPath, os.Stderr, translator)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.invalid_runtime_arguments", err))
		os.Exit(2)
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.load_config", err))
		os.Exit(1)
	}
	startupConfig := cfg
	if opts.controlPort != 0 {
		cfg.ControlPort = opts.controlPort
	}

	logger, err := newLogger(cfg.Log, opts.logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.init_logger", err))
		os.Exit(1)
	}
	slog.SetDefault(logger)
	if cfg.UsedDeprecatedControlListenAddr {
		logger.Warn("control_listen_addr is deprecated; replace it with control_port", "component", "config", "control_port", cfg.ControlPort)
	}
	if len(cfg.UnknownFields) > 0 {
		logger.Warn("config contains unknown keys; they are preserved but ignored and a misspelled policy key silently loses its policy",
			"component", "config", "keys", strings.Join(cfg.UnknownFields, ", "))
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
			printRuntimeReadyWithTranslator(os.Stdout, info, translator)
		}
	}
	if err := runForwardingWithReporter(ctx, cfg, startupConfig, opts.configPath, logger, reporter); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.runtime_failed", err))
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
	quotaPath := quotaFilePath(cfg, configPath)
	if samePath, err := statsstore.SameFilePath(quotaPath, configPath); err != nil {
		return fmt.Errorf("quota state path: %w", err)
	} else if samePath {
		return fmt.Errorf("quota state path must differ from config path: %s", quotaPath)
	}
	if cfg.Stats.Persist {
		statsPath := statsFilePath(cfg, configPath)
		if samePath, err := statsstore.SameFilePath(quotaPath, statsPath); err != nil {
			return fmt.Errorf("quota state path: %w", err)
		} else if samePath {
			return fmt.Errorf("quota state path must differ from stats path: %s", quotaPath)
		}
	}
	quotaLedger, err := engine.NewQuotaLedger(statsstore.NewQuotaStore(quotaPath))
	if err != nil {
		return fmt.Errorf("load quota state: %w", err)
	}
	if configHasTrafficQuota(cfg) {
		if err := quotaLedger.SaveExact(); err != nil {
			return fmt.Errorf("initialize quota state: %w", err)
		}
	}
	manager := engine.NewManagerWithOptions(collector, engine.ManagerOptions{UDPMaxSessions: cfg.UDPMaxSessions, QuotaLedger: quotaLedger})

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
		HostNetwork:   netinfo.Collect,
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
	if err := quotaLedger.SaveExact(); err != nil {
		logger.Warn("quota state shutdown save failed", "component", "quota", "error", err)
	}
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

func quotaFilePath(cfg config.File, configPath string) string {
	return filepath.Join(filepath.Dir(statsFilePath(cfg, configPath)), statsstore.DefaultQuotaFilename)
}

func configHasTrafficQuota(cfg config.File) bool {
	for _, rule := range cfg.Rules {
		if rule.TrafficQuota != nil {
			return true
		}
	}
	return false
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
	runCtlWithTranslator(args, defaultTranslator())
}

func runCtlWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("ctl", flag.ExitOnError)
	defaults := loadManagementDefaultsWithTranslator(os.Stderr, translator)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), translator.T("cli.ctl.usage"))
		fs.PrintDefaults()
	}
	addr := fs.String("addr", defaults.Address, translator.T("cli.ctl.flag.address"))
	token := fs.String("token", defaults.Token, translator.T("cli.ctl.flag.token"))
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	localizeControlClientFlags(fs, translator)
	fs.Parse(args)

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, translator.T("cli.ctl.usage_short"))
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
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.ctl.error.unknown_action", cmdArgs[0]))
		os.Exit(1)
	}

	status, body, err := doRequest(*addr, *token, tlsFlags.Opts(), *headerFlags, method, path, reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.ctl.error.request", err))
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
	runTUIWithTranslator(args, defaultTranslator())
}

func runTUIWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	defaults := loadManagementDefaultsWithTranslator(os.Stderr, translator)
	addr := fs.String("addr", defaults.Address, translator.T("cli.tui.flag.address"))
	token := fs.String("token", defaults.Token, translator.T("cli.tui.flag.token"))
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	localizeControlClientFlags(fs, translator)
	fs.Parse(args)
	addressExplicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "addr" {
			addressExplicit = true
		}
	})

	var httpClient *http.Client
	if tlsFlags.Opts().Any() {
		hc, err := controlapi.NewHTTPClient(tlsFlags.Opts(), 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.tls", err))
			os.Exit(1)
		}
		httpClient = hc
	}

	options := tui.RunOptions{Translator: translator}
	if localServiceControlEligible(*addr, addressExplicit, os.Getenv) {
		configPath := strings.TrimSpace(defaults.ConfigPath)
		if configPath == "" {
			configPath, _ = defaultRuntimeConfigPath()
		}
		controller := newTUIServiceController(translator, configPath)
		if !localTUIServiceReady(context.Background(), os.Stderr, translator, controller) {
			os.Exit(1)
		}
		options.ServiceController = controller
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := tui.RunWithOptions(ctx, os.Stdout, *addr, *token, httpClient, headerFlags.HTTPHeader(), options); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.error.tui", err))
		os.Exit(1)
	}
}

// ── version ─────────────────────────────────────────────────────────

func runVersion(args []string) {
	runVersionWithTranslator(args, defaultTranslator())
}

func runVersionWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, translator.T("cli.version.flag.json"))
	fs.Parse(args)

	_ = writeVersion(os.Stdout, asJSON, translator)
}

type versionInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
}

func writeVersion(w io.Writer, asJSON bool, translator *i18n.Translator) error {
	translator = translatorOrEnglish(translator)
	if asJSON {
		info := versionInfo{
			Name:      "vmflow",
			Version:   version,
			Commit:    optionalField(commit, "none"),
			BuildTime: optionalField(buildTime, "unknown"),
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	fmt.Fprintf(w, "vmflow %s\n", version)
	if commit != "none" && commit != "" {
		fmt.Fprintf(w, "%-11s %s\n", translator.T("cli.version.commit"), commit)
	}
	if buildTime != "unknown" && buildTime != "" {
		fmt.Fprintf(w, "%-11s %s\n", translator.T("cli.version.built"), buildTime)
	}
	return nil
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
	runUpdateWithTranslator(args, defaultTranslator())
}

func runUpdateWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), translator.T("cli.update.usage"))
		fs.PrintDefaults()
	}
	var checkOnly bool
	var targetVersion string
	fs.BoolVar(&checkOnly, "check", false, translator.T("cli.update.flag.check"))
	fs.StringVar(&targetVersion, "version", "", translator.T("cli.update.flag.version"))
	fs.Parse(args)
	if len(fs.Args()) != 0 {
		fmt.Fprintln(os.Stderr, translator.T("cli.update.error.positionals"))
		os.Exit(1)
	}
	if !checkOnly && !updater.SelfUpdateSupported() {
		fmt.Fprintln(os.Stderr, translator.T("cli.update.error.windows"))
		os.Exit(1)
	}

	currentRaw := strings.TrimSpace(version)
	if !checkOnly && targetVersion == "" && strings.EqualFold(currentRaw, "dev") {
		fmt.Fprintln(os.Stderr, translator.Tf("cli.update.error.tagged_build", version))
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
		fmt.Fprintln(os.Stderr, translator.Tf("cli.update.error.check", err))
		os.Exit(1)
	}
	if result == nil {
		fmt.Fprintln(os.Stderr, translator.T("cli.update.error.empty_result"))
		os.Exit(1)
	}

	if checkOnly {
		writeUpdateCheckWithTranslator(os.Stdout, result, targetTag != "", translator)
		return
	}

	if !result.UpdateAvailable {
		if targetTag != "" && normalizeReleaseTag(result.CurrentVersion) == normalizeReleaseTag(result.LatestVersion) {
			fmt.Fprintln(os.Stdout, translator.Tf("cli.update.output.requested_current", formatReleaseTag(result.LatestVersion)))
			return
		}
		if targetTag != "" {
			fmt.Fprintln(os.Stdout, translator.Tf("cli.update.output.requested_not_newer", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion)))
			return
		}
		fmt.Fprintln(os.Stdout, translator.Tf("cli.update.output.current", formatReleaseTag(result.CurrentVersion)))
		return
	}

	if result.Release == nil {
		fmt.Fprintln(os.Stderr, translator.T("cli.update.error.release_unavailable"))
		os.Exit(1)
	}

	fmt.Fprintln(os.Stdout, translator.Tf("cli.update.output.updating", formatReleaseTag(result.CurrentVersion), formatReleaseTag(result.LatestVersion)))
	if err := client.DownloadAndInstall(ctx, result.Release, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, translator.Tf("cli.update.error.install", err))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, translator.Tf("cli.update.output.success", formatReleaseTag(result.LatestVersion)))
}

func writeUpdateCheck(w io.Writer, result *updater.CheckResult, specific bool) {
	writeUpdateCheckWithTranslator(w, result, specific, defaultTranslator())
}

func writeUpdateCheckWithTranslator(w io.Writer, result *updater.CheckResult, specific bool, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	switch {
	case result.UpdateAvailable && specific:
		fmt.Fprintln(w, translator.Tf("cli.update.output.target_available", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion)))
	case result.UpdateAvailable:
		fmt.Fprintln(w, translator.Tf("cli.update.output.available", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion)))
	case specific && normalizeReleaseTag(result.CurrentVersion) == normalizeReleaseTag(result.LatestVersion):
		fmt.Fprintln(w, translator.Tf("cli.update.output.requested_current", formatReleaseTag(result.LatestVersion)))
	case specific:
		fmt.Fprintln(w, translator.Tf("cli.update.output.requested_not_newer", formatReleaseTag(result.LatestVersion), formatReleaseTag(result.CurrentVersion)))
	default:
		fmt.Fprintln(w, translator.Tf("cli.update.output.current", formatReleaseTag(result.CurrentVersion)))
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
	runServiceWithTranslator(args, defaultTranslator())
}

func runServiceWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), translator.T("cli.service.usage"))
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", translator.T("cli.service.flag.config"))
	user := fs.String("user", "", translator.T("cli.service.flag.user"))
	logFile := fs.String("log-file", "", translator.T("cli.service.flag.log_file"))
	controlPort := fs.Int("control-port", 0, translator.T("cli.service.flag.control_port"))
	var extraArgs serviceArgList
	fs.Var(&extraArgs, "extra-arg", translator.T("cli.service.flag.extra_arg"))
	binaryPath := fs.String("binary", "", translator.T("cli.service.flag.binary"))

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, translator.T("cli.service.usage_short"))
		os.Exit(1)
	}
	// The action is the first positional; flags follow it. The flag package
	// stops parsing at the first non-flag, so peel the action off before Parse.
	action := args[0]
	if strings.HasPrefix(action, "-") {
		fmt.Fprintln(os.Stderr, translator.T("cli.service.usage_short"))
		os.Exit(1)
	}
	fs.Parse(args[1:])
	if *controlPort < 0 || *controlPort > 65535 {
		fmt.Fprintln(os.Stderr, translator.T("cli.error.control_port"))
		os.Exit(2)
	}
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintln(os.Stderr, translator.Tf("cli.error.unexpected_arguments", extra))
		os.Exit(1)
	}

	cfg := service.Config{
		Translator:  translator,
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
	case "start":
		err = service.Start(cfg, os.Stdout)
	case "uninstall":
		err = service.Uninstall(cfg, os.Stdout)
	case "status":
		err = service.Status(cfg, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, translator.Tf("cli.service.error.unknown_action", action))
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, translator.Tf("cli.service.error.failed", action, err))
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
