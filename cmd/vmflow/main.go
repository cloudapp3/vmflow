package main

import (
	"context"
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
	"strings"
	"syscall"
	"time"

	"github.com/cloudapp3/vmflow/bot"
	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/logging"
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
  vmflow daemon        [-config path] [-admin-listen addr]              Start forwarding daemon
  vmflow ctl           [-addr url] [-token token] <health|rules|stats|metrics|precheck|reload>    Query running daemon
  vmflow tui           [-addr url] [-token token]                      Terminal UI dashboard
  vmflow version       [-json]                                         Show version info

Aliases: daemon=d, ctl=c, tui=t, version=v
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
	adminListen := fs.String("admin-listen", "", "override admin listen addr")
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
	if strings.TrimSpace(*adminListen) != "" {
		cfg.AdminListenAddr = strings.TrimSpace(*adminListen)
	}

	logger, err := logging.New(cfg.Log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger failed: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)
	warnIfUnsafeAdmin(cfg, logger)

	collector := engine.NewCollector()
	manager := engine.NewManager(collector)

	metricsCollector := metrics.New(manager)
	result := manager.ApplySnapshot(cfg.Rules, engine.ApplySnapshotOptions{ReplaceAll: true})
	metricsCollector.ObserveApplyResult(result)
	if result.FailedRules > 0 {
		payload, _ := json.MarshalIndent(result, "", "  ")
		logger.Error("initial apply failed", "component", "engine", "event", "initial_apply_failed", "result", string(payload))
		manager.StopAll()
		os.Exit(1)
	}
	logger.Info("initial snapshot applied", "component", "engine", "event", "initial_apply", "rule_count", len(cfg.Rules), "applied_rules", result.AppliedRules, "stopped_rules", result.StoppedRules)

	runtime := &controlapi.Runtime{
		ConfigPath: *configPath,
		Manager:    manager,
		Logger:     logger,
		Auth:       controlapi.NewAuthenticator(cfg.Auth),
		Metrics:    metricsCollector,
	}
	server := &http.Server{
		Addr:              cfg.AdminListenAddr,
		Handler:           controlapi.NewHandler(runtime),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("vmflow admin server listening", "component", "daemon", "event", "admin_listen", "addr", cfg.AdminListenAddr)
		errCh <- server.ListenAndServe()
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received", "component", "daemon", "event", "shutdown_signal")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "component", "daemon", "event", "server_failed", "error", err)
			if botCancel != nil {
				botCancel()
			}
			manager.StopAll()
			os.Exit(1)
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
}

func warnIfUnsafeAdmin(cfg config.File, logger *slog.Logger) {
	if logger == nil || cfg.Auth.Enabled {
		return
	}
	host, _, err := net.SplitHostPort(cfg.AdminListenAddr)
	if err != nil {
		logger.Warn("admin api auth is disabled", "component", "daemon", "event", "auth_disabled", "admin_listen_addr", cfg.AdminListenAddr)
		return
	}
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" {
		logger.Warn("admin api is exposed without auth", "component", "daemon", "event", "auth_disabled_exposed", "admin_listen_addr", cfg.AdminListenAddr)
		return
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return
	}
	if strings.EqualFold(host, "localhost") {
		return
	}
	logger.Warn("admin api auth is disabled on non-loopback address", "component", "daemon", "event", "auth_disabled_non_loopback", "admin_listen_addr", cfg.AdminListenAddr)
}

// ── ctl ─────────────────────────────────────────────────────────────

func runCtl(args []string) {
	fs := flag.NewFlagSet("ctl", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:19090", "admin api base url")
	token := fs.String("token", os.Getenv("VMFLOW_ADMIN_TOKEN"), "admin api bearer token (or VMFLOW_ADMIN_TOKEN)")
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

	status, body, err := doRequest(*addr, *token, method, path, reqBody)
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

func doRequest(baseURL, token, method, path, body string) (int, []byte, error) {
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
	resp, err := http.DefaultClient.Do(req)
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
	addr := fs.String("addr", "http://127.0.0.1:19090", "relayd admin API address")
	token := fs.String("token", os.Getenv("VMFLOW_ADMIN_TOKEN"), "admin api bearer token (or VMFLOW_ADMIN_TOKEN)")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := tui.Run(ctx, os.Stdout, *addr, *token); err != nil {
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
