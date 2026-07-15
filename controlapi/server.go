package controlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	// Bot controls the Telegram bot lifecycle at runtime. May be nil when bot
	// support is disabled; bot config endpoints report unavailable then.
	Bot BotController
	// StartupConfig is the normalized file configuration used to start the
	// daemon. Reload uses it to reject changes to restart-only fields.
	StartupConfig *config.File
	limiterInst   *ipLimiter
	reloadMu      sync.Mutex
	stateMu       sync.RWMutex
	degraded      bool
	degradedCause string
	configHooks   *configManagementHooks
}

// RestartRequiredError reports configuration fields that cannot be changed by
// hot reload without leaving the API response out of sync with the daemon.
type RestartRequiredError struct {
	Fields []string `json:"fields"`
}

func (err *RestartRequiredError) Error() string {
	return "configuration changes require daemon restart: " + strings.Join(err.Fields, ", ")
}

type ReloadApplyError struct {
	Transaction engine.TransactionalApplyResult `json:"transaction"`
}

func (err *ReloadApplyError) Error() string {
	if err == nil || err.Transaction.ApplyFailure == nil {
		return "reload apply failed"
	}
	return "reload apply failed: " + err.Transaction.ApplyFailure.Error
}

func NewHandler(runtime *Runtime) http.Handler {
	if runtime.limiterInst == nil {
		runtime.limiterInst = newIPLimiter()
	}
	mux := http.NewServeMux()
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
	runtime.registerConfigManagementHandlers(mux)
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
				"error":  "configuration could not be loaded",
				"result": precheckResultFromError(),
			})
			return
		}
		status := http.StatusOK
		if !result.OK {
			status = http.StatusBadRequest
		}
		runtime.log(r).Info("precheck completed", "component", "controlapi", "event", "precheck", "ok", result.OK, "rule_count", len(cfg.Rules), "error_count", result.ErrorCount, "warning_count", result.WarningCount)
		writeJSON(w, status, map[string]any{
			"config_path": filepath.Base(strings.TrimSpace(runtime.ConfigPath)),
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
				runtime.log(r).Warn("control reload blocked by precheck", "component", "controlapi", "event", "reload_precheck_failed", "error_count", precheckErr.Result.ErrorCount, "warning_count", precheckErr.Result.WarningCount)
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "reload blocked by precheck", "precheck": precheckErr.Result})
				return
			}
			var restartErr *RestartRequiredError
			if errors.As(err, &restartErr) {
				runtime.log(r).Warn("control reload requires daemon restart", "component", "controlapi", "event", "reload_restart_required", "fields", restartErr.Fields)
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":  "configuration changes require daemon restart",
					"fields": restartErr.Fields,
				})
				return
			}
			var applyErr *ReloadApplyError
			if errors.As(err, &applyErr) {
				runtime.metrics().ObserveApplyResult(applyErr.Transaction.Apply)
				status := http.StatusInternalServerError
				if applyErr.Transaction.Rollback.Failed {
					status = http.StatusServiceUnavailable
				}
				runtime.log(r).Error("control reload transaction failed", "component", "controlapi", "event", "reload_apply_failed", "rollback_failed", applyErr.Transaction.Rollback.Failed)
				writeJSON(w, status, map[string]any{
					"error":       "reload applied with rule failures",
					"transaction": applyErr.Transaction,
				})
				return
			}
			if result.FailedRules > 0 {
				runtime.metrics().ObserveApplyResult(result)
				runtime.log(r).Error("control reload partially failed", "component", "controlapi", "event", "reload_apply_failed", "error", err, "applied_rules", result.AppliedRules, "stopped_rules", result.StoppedRules, "failed_rules", result.FailedRules)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":  "reload applied with rule failures",
					"result": result,
				})
				return
			}
			runtime.log(r).Error("control reload failed", "component", "controlapi", "event", "reload_failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "reload failed; see daemon logs"})
			return
		}
		runtime.metrics().ObserveReload("ok")
		runtime.metrics().ObserveApplyResult(result)
		runtime.log(r).Info("control reload completed", "component", "controlapi", "event", "reload", "rule_count", len(cfg.Rules), "applied_rules", result.AppliedRules, "stopped_rules", result.StoppedRules, "failed_rules", result.FailedRules)
		writeJSON(w, http.StatusOK, map[string]any{
			"config_path":      filepath.Base(strings.TrimSpace(runtime.ConfigPath)),
			"control_port":     cfg.ControlPort,
			"udp_max_sessions": cfg.UDPMaxSessions,
			"rule_count":       len(cfg.Rules),
			"applied_fields":   []string{"rules", "udp_max_sessions"},
			"result":           result,
		})
	})
	return runtime.withMiddleware(mux)
}

func (runtime *Runtime) Reload() (config.File, engine.ApplyResult, error) {
	if runtime == nil || runtime.Manager == nil {
		return config.File{}, engine.ApplyResult{}, fmt.Errorf("runtime unavailable")
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()

	cfg, checkResult, err := runtime.Precheck()
	if err != nil {
		return config.File{}, engine.ApplyResult{}, err
	}
	if !checkResult.OK {
		return cfg, engine.ApplyResult{}, &precheck.Error{Result: checkResult}
	}
	if fields := runtime.restartRequiredFields(cfg); len(fields) > 0 {
		return cfg, engine.ApplyResult{}, &RestartRequiredError{Fields: fields}
	}
	previousLimit, _ := runtime.Manager.UDPMaxSessions()
	limitLoweredBeforeApply := cfg.UDPMaxSessions < previousLimit
	if limitLoweredBeforeApply {
		// Tighten admission before changing rules so a successful reload cannot
		// create sessions above the new limit during the apply window.
		runtime.Manager.SetUDPMaxSessions(cfg.UDPMaxSessions)
	}
	transaction := runtime.Manager.ApplySnapshotTransactional(cfg.Rules, engine.ApplySnapshotOptions{ReplaceAll: true})
	result := transaction.Apply
	if transaction.ApplyFailure != nil {
		if limitLoweredBeforeApply {
			runtime.Manager.SetUDPMaxSessions(previousLimit)
		}
		if transaction.Rollback.Failed {
			runtime.markDegraded("reload rule rollback failed")
		} else {
			runtime.markDegraded("configuration reload was not applied; previous runtime restored")
		}
		return cfg, result, &ReloadApplyError{Transaction: transaction}
	}
	if cfg.UDPMaxSessions > previousLimit {
		// Do not relax admission until every rule was applied successfully.
		runtime.Manager.SetUDPMaxSessions(cfg.UDPMaxSessions)
	}
	runtime.clearDegraded()
	return cfg, result, nil
}

func (runtime *Runtime) restartRequiredFields(next config.File) []string {
	if runtime == nil || runtime.StartupConfig == nil {
		return nil
	}
	current := *runtime.StartupConfig
	fields := make([]string, 0, 8)
	if current.ControlPort != next.ControlPort {
		fields = append(fields, "control_port")
	}
	if !reflect.DeepEqual(current.ControlTLS, next.ControlTLS) {
		fields = append(fields, "control_tls")
	}
	if !reflect.DeepEqual(current.Log, next.Log) {
		fields = append(fields, "log")
	}
	if !reflect.DeepEqual(current.Auth, next.Auth) {
		fields = append(fields, "auth")
	}
	currentBot := botSettingsFromConfig(current)
	nextBot := botSettingsFromConfig(next)
	if currentBot.Token != nextBot.Token {
		fields = append(fields, "bot_token")
	}
	if currentBot.ChatID != nextBot.ChatID {
		fields = append(fields, "bot_chat")
	}
	if currentBot.ControlToken != nextBot.ControlToken {
		fields = append(fields, "bot_control_token")
	}
	if current.AcmeChallenge != next.AcmeChallenge ||
		current.AcmeHTTP01Addr != next.AcmeHTTP01Addr ||
		current.AcmeCacheDir != next.AcmeCacheDir ||
		!reflect.DeepEqual(current.AcmeDNS01, next.AcmeDNS01) {
		fields = append(fields, "acme")
	}
	if current.CertCacheDir != next.CertCacheDir {
		fields = append(fields, "cert_cache_dir")
	}
	if !reflect.DeepEqual(current.CertReview, next.CertReview) {
		fields = append(fields, "cert_review")
	}
	if !reflect.DeepEqual(current.Stats, next.Stats) {
		fields = append(fields, "stats")
	}
	return fields
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

		ip := clientIP(r)
		if runtime.limiter().locked(ip) {
			statusWriter.status = http.StatusTooManyRequests
			writeJSON(statusWriter, http.StatusTooManyRequests, map[string]any{"error": "too many failed auth attempts; try again later"})
			runtime.log(nil).Warn("control auth rate limited", "component", "controlapi", "event", "auth_rate_limited", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			duration := time.Since(started)
			runtime.metrics().ObserveControlRequest(r.Method, r.URL.Path, statusWriter.status, duration)
			runtime.logRequest(r, statusWriter.status, duration, AuthInfo{})
			return
		}

		info, ok := runtime.authenticator().Authenticate(r)
		runtime.limiter().note(ip, ok)
		if !ok {
			statusWriter.status = http.StatusUnauthorized
			writeJSON(statusWriter, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			runtime.log(nil).Warn("control authentication failed", "component", "controlapi", "event", "auth_failed", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			duration := time.Since(started)
			runtime.metrics().ObserveControlRequest(r.Method, r.URL.Path, statusWriter.status, duration)
			runtime.logRequest(r, statusWriter.status, duration, AuthInfo{})
			return
		}

		next.ServeHTTP(statusWriter, r.WithContext(withAuthInfo(r.Context(), info)))
		duration := time.Since(started)
		runtime.metrics().ObserveControlRequest(r.Method, r.URL.Path, statusWriter.status, duration)
		runtime.logRequest(r, statusWriter.status, duration, info)
	})
}

// clientIP returns the request's peer address (without port). It deliberately
// ignores X-Forwarded-For (we do not trust client-provided headers), so behind
// a proxy this is the proxy's address.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter returns the per-runtime failed-auth throttle, initializing it on
// first use.
func (runtime *Runtime) limiter() *ipLimiter {
	if runtime == nil {
		return nil
	}
	if runtime.limiterInst == nil {
		runtime.limiterInst = newIPLimiter()
	}
	return runtime.limiterInst
}

func (runtime *Runtime) authorizeWrite(w http.ResponseWriter, r *http.Request) bool {
	info, ok := AuthInfoFromContext(r.Context())
	if !ok || !info.canWrite() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		runtime.log(r).Warn("control authorization denied", "component", "controlapi", "event", "auth_denied", "method", r.Method, "path", r.URL.Path)
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
		logger.Error("control request", attrs...)
		return
	}
	if status >= 400 {
		logger.Warn("control request", attrs...)
		return
	}
	logger.Debug("control request", attrs...)
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func precheckResultFromError() precheck.Result {
	result := precheck.Result{
		OK:           false,
		ErrorCount:   1,
		CheckedRules: 0,
		Items: []precheck.Item{{
			Severity: precheck.SeverityError,
			Check:    "config_load",
			Message:  "configuration could not be loaded",
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
