package controlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/certreview"
	"github.com/cloudapp3/vmflow/certstore"
	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/metrics"
	"github.com/cloudapp3/vmflow/precheck"
)

type Runtime struct {
	ConfigPath      string
	Manager         *engine.Manager
	Logger          *slog.Logger
	Auth            *Authenticator
	Metrics         *metrics.Collector
	PrecheckOptions *precheck.Options
	CertStore       *certstore.Store
	CertReviewer    *certreview.Reviewer
}

func NewHandler(runtime *Runtime) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"running_rules": runtime.runningRules(),
			"time":          time.Now().Unix(),
		})
	})
	mux.HandleFunc("/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		rules := runtime.rules()
		writeJSON(w, http.StatusOK, map[string]any{"items": rules})
	})
	mux.HandleFunc("/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": runtime.snapshots(),
		})
	})
	mux.HandleFunc("/v1/precheck", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		cfg, result, err := runtime.Precheck()
		if err != nil {
			runtime.log(r).Error("precheck failed to load config", "component", "controlapi", "event", "precheck_load_failed", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":     false,
				"error":  err.Error(),
				"result": precheckResultFromError(err),
			})
			return
		}
		status := http.StatusOK
		if !result.OK {
			status = http.StatusBadRequest
		}
		runtime.log(r).Info("precheck completed", "component", "controlapi", "event", "precheck", "ok", result.OK, "rule_count", len(cfg.Rules), "error_count", result.ErrorCount, "warning_count", result.WarningCount)
		writeJSON(w, status, map[string]any{
			"config_path": strings.TrimSpace(runtime.ConfigPath),
			"rule_count":  len(cfg.Rules),
			"result":      result,
		})
	})
	mux.Handle("/metrics", runtime.metricsHandler())
	mux.HandleFunc("/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !runtime.authorizeWrite(w, r) {
			return
		}
		cfg, result, err := runtime.Reload()
		if err != nil {
			runtime.metrics().ObserveReload("failed")
			var precheckErr *precheck.Error
			if errors.As(err, &precheckErr) {
				runtime.log(r).Warn("admin reload blocked by precheck", "component", "controlapi", "event", "reload_precheck_failed", "error_count", precheckErr.Result.ErrorCount, "warning_count", precheckErr.Result.WarningCount)
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "precheck": precheckErr.Result})
				return
			}
			runtime.log(r).Error("admin reload failed", "component", "controlapi", "event", "reload_failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadStatus := "ok"
		if result.FailedRules > 0 {
			reloadStatus = "failed"
		}
		runtime.metrics().ObserveReload(reloadStatus)
		runtime.metrics().ObserveApplyResult(result)
		runtime.log(r).Info("admin reload completed", "component", "controlapi", "event", "reload", "rule_count", len(cfg.Rules), "applied_rules", result.AppliedRules, "stopped_rules", result.StoppedRules, "failed_rules", result.FailedRules)
		writeJSON(w, http.StatusOK, map[string]any{
			"config_path":       strings.TrimSpace(runtime.ConfigPath),
			"admin_listen_addr": cfg.AdminListenAddr,
			"rule_count":        len(cfg.Rules),
			"result":            result,
		})
	})
	return runtime.withMiddleware(mux)
}

func (runtime *Runtime) Reload() (config.File, engine.ApplyResult, error) {
	if runtime == nil || runtime.Manager == nil {
		return config.File{}, engine.ApplyResult{}, fmt.Errorf("runtime unavailable")
	}
	cfg, checkResult, err := runtime.Precheck()
	if err != nil {
		return config.File{}, engine.ApplyResult{}, err
	}
	if !checkResult.OK {
		return cfg, engine.ApplyResult{}, &precheck.Error{Result: checkResult}
	}
	result := runtime.Manager.ApplySnapshot(cfg.Rules, engine.ApplySnapshotOptions{ReplaceAll: true})
	return cfg, result, nil
}

func (runtime *Runtime) Precheck() (config.File, precheck.Result, error) {
	if runtime == nil || runtime.Manager == nil {
		return config.File{}, precheck.Result{}, fmt.Errorf("runtime unavailable")
	}
	cfg, err := config.Load(runtime.ConfigPath)
	if err != nil {
		return config.File{}, precheck.Result{}, err
	}
	result := precheck.CheckConfig(cfg, runtime.Manager.RunningRules(), runtime.precheckOptions())
	return cfg, result, nil
}

func (runtime *Runtime) precheckOptions() precheck.Options {
	if runtime != nil && runtime.PrecheckOptions != nil {
		return *runtime.PrecheckOptions
	}
	return precheck.DefaultOptions()
}

func (runtime *Runtime) rules() []engine.Rule {
	if runtime == nil || runtime.Manager == nil {
		return nil
	}
	return runtime.Manager.RunningRules()
}

func (runtime *Runtime) runningRules() int {
	if runtime == nil || runtime.Manager == nil {
		return 0
	}
	return runtime.Manager.RunningCount()
}

func (runtime *Runtime) snapshots() []engine.TrafficSnapshot {
	if runtime == nil || runtime.Manager == nil {
		return nil
	}
	return runtime.Manager.SnapshotAll()
}

func (runtime *Runtime) metricsHandler() http.Handler {
	return runtime.metrics().Handler()
}

func (runtime *Runtime) metrics() *metrics.Collector {
	if runtime == nil {
		return metrics.New(nil)
	}
	if runtime.Metrics == nil {
		runtime.Metrics = metrics.New(runtime.Manager)
	}
	return runtime.Metrics
}

func (runtime *Runtime) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		statusWriter := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}

		info, ok := runtime.authenticator().Authenticate(r)
		if !ok {
			statusWriter.status = http.StatusUnauthorized
			writeJSON(statusWriter, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			runtime.log(nil).Warn("admin authentication failed", "component", "controlapi", "event", "auth_failed", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			duration := time.Since(started)
			runtime.metrics().ObserveAdminRequest(r.Method, r.URL.Path, statusWriter.status, duration)
			runtime.logRequest(r, statusWriter.status, duration, AuthInfo{})
			return
		}

		next.ServeHTTP(statusWriter, r.WithContext(withAuthInfo(r.Context(), info)))
		duration := time.Since(started)
		runtime.metrics().ObserveAdminRequest(r.Method, r.URL.Path, statusWriter.status, duration)
		runtime.logRequest(r, statusWriter.status, duration, info)
	})
}

func (runtime *Runtime) authorizeWrite(w http.ResponseWriter, r *http.Request) bool {
	info, ok := AuthInfoFromContext(r.Context())
	if !ok || !info.canWrite() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		runtime.log(r).Warn("admin authorization denied", "component", "controlapi", "event", "auth_denied", "method", r.Method, "path", r.URL.Path)
		return false
	}
	return true
}

func (runtime *Runtime) authenticator() *Authenticator {
	if runtime == nil || runtime.Auth == nil {
		return NewAuthenticator(config.AuthConfig{})
	}
	return runtime.Auth
}

func (runtime *Runtime) log(r *http.Request) *slog.Logger {
	logger := slog.Default()
	if runtime != nil && runtime.Logger != nil {
		logger = runtime.Logger
	}
	if r != nil {
		if info, ok := AuthInfoFromContext(r.Context()); ok {
			logger = logger.With("actor", info.Name, "role", info.Role, "remote_addr", r.RemoteAddr)
		}
	}
	return logger
}

func (runtime *Runtime) logRequest(r *http.Request, status int, duration time.Duration, info AuthInfo) {
	logger := slog.Default()
	if runtime != nil && runtime.Logger != nil {
		logger = runtime.Logger
	}
	attrs := []any{
		"component", "controlapi",
		"event", "request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"duration_ms", duration.Milliseconds(),
		"remote_addr", r.RemoteAddr,
	}
	if info.Name != "" {
		attrs = append(attrs, "actor", info.Name, "role", info.Role)
	}
	if status >= 500 {
		logger.Error("admin request", attrs...)
		return
	}
	if status >= 400 {
		logger.Warn("admin request", attrs...)
		return
	}
	logger.Debug("admin request", attrs...)
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func precheckResultFromError(err error) precheck.Result {
	result := precheck.Result{
		OK:           false,
		ErrorCount:   1,
		CheckedRules: 0,
		Items: []precheck.Item{{
			Severity: precheck.SeverityError,
			Check:    "config_load",
			Message:  err.Error(),
		}},
	}
	return result
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		_, _ = w.Write([]byte(`{"error":"marshal failed"}`))
		return
	}
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n"))
}
