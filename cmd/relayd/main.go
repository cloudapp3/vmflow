package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
	controlPort := flag.Int("control-port", 0, "override the local management port")
	flag.Parse()

	if strings.TrimSpace(*configPath) == "" {
		fmt.Fprintln(os.Stderr, "missing -config")
		os.Exit(1)
	}
	if *controlPort < 0 || *controlPort > 65535 {
		fmt.Fprintln(os.Stderr, "control-port must be 0 (use config) or between 1 and 65535")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	startupConfig := cfg
	if *controlPort != 0 {
		cfg.ControlPort = *controlPort
	}

	logger, err := logging.New(cfg.Log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger failed: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)
	if cfg.UsedDeprecatedControlListenAddr {
		logger.Warn("control_listen_addr is deprecated; replace it with control_port", "component", "config", "control_port", cfg.ControlPort)
	}

	tlsCfg, err := controlapi.BuildServerTLSConfig(cfg.ControlTLS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control api tls: %v\n", err)
		os.Exit(1)
	}

	manager := engine.NewManagerWithOptions(engine.NewCollector(), engine.ManagerOptions{UDPMaxSessions: cfg.UDPMaxSessions})
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
		ConfigPath:    *configPath,
		Manager:       manager,
		Logger:        logger,
		Auth:          controlapi.NewAuthenticator(cfg.Auth),
		Metrics:       metricsCollector,
		StartupConfig: &startupConfig,
	}
	server := &http.Server{
		Addr:              cfg.ControlListenAddress(),
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
		logger.Info("relayd control server listening", "component", "daemon", "event", "control_listen", "addr", cfg.ControlListenAddress(), "scheme", scheme, "mtls", tlsCfg != nil && tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert)
		errCh <- listen()
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
