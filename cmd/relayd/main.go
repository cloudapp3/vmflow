package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/logging"
	"github.com/cloudapp3/vmflow/metrics"
)

func main() {
	configPath := flag.String("config", "", "config file path")
	adminListen := flag.String("admin-listen", "", "override admin listen addr")
	flag.Parse()

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

	manager := engine.NewManager(engine.NewCollector())
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
		logger.Info("relayd admin server listening", "component", "daemon", "event", "admin_listen", "addr", cfg.AdminListenAddr)
		errCh <- server.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received", "component", "daemon", "event", "shutdown_signal")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "component", "daemon", "event", "server_failed", "error", err)
			manager.StopAll()
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	manager.StopAll()
	logger.Info("relayd stopped", "component", "daemon", "event", "stopped")
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
