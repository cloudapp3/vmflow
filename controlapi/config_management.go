package controlapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

const maxConfigManagementBodyBytes = 1 << 20

var errMissingConfigRevision = errors.New("missing If-Match header")

type sessionCapabilities struct {
	RulesWrite bool `json:"rules_write"`
}

type sessionResponse struct {
	Actor        string              `json:"actor"`
	Role         string              `json:"role"`
	Capabilities sessionCapabilities `json:"capabilities"`
}

type rulesConfigRequest struct {
	UDPMaxSessions *int           `json:"udp_max_sessions"`
	Rules          *[]engine.Rule `json:"rules"`
}

type rulesConfigResponse struct {
	Revision       string        `json:"revision"`
	Writable       bool          `json:"writable"`
	UDPMaxSessions int           `json:"udp_max_sessions"`
	Rules          []engine.Rule `json:"rules"`
}

type configRuleDiff struct {
	RuleID        string `json:"rule_id"`
	ConfigAction  string `json:"config_action"`
	RuntimeAction string `json:"runtime_action"`
}

type rulesPrecheckResponse struct {
	Revision              string           `json:"revision"`
	UDPMaxSessionsChanged bool             `json:"udp_max_sessions_changed"`
	Diff                  []configRuleDiff `json:"diff"`
	Precheck              precheck.Result  `json:"precheck"`
}

type configManagementHooks struct {
	BeforeCommit func(*stagedConfig)
}

type configRevisionError struct {
	Expected string
	Actual   string
}

func (err *configRevisionError) Error() string {
	return "configuration revision changed"
}

type rulesValidationError struct {
	Err error
}

func (err *rulesValidationError) Error() string {
	return err.Err.Error()
}

func (err *rulesValidationError) Unwrap() error {
	return err.Err
}

func (runtime *Runtime) registerConfigManagementHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/v1/session", runtime.handleSession)
	mux.HandleFunc("/v1/config/rules", runtime.handleRulesConfig)
	mux.HandleFunc("/v1/config/rules/precheck", runtime.handleRulesConfigPrecheck)
	mux.HandleFunc("/v1/config/bot", runtime.handleBotConfig)
	mux.HandleFunc("/v1/bot/start", runtime.handleBotStart)
	mux.HandleFunc("/v1/bot/stop", runtime.handleBotStop)
}

func (runtime *Runtime) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	info, ok := AuthInfoFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	writable := runtime.authenticator().Enabled() && info.canWrite()
	writeJSON(w, http.StatusOK, sessionResponse{
		Actor: info.Name,
		Role:  info.Role,
		Capabilities: sessionCapabilities{
			RulesWrite: writable,
		},
	})
}

func (runtime *Runtime) handleRulesConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runtime.getRulesConfig(w, r)
	case http.MethodPut:
		runtime.putRulesConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (runtime *Runtime) getRulesConfig(w http.ResponseWriter, r *http.Request) {
	document, err := loadConfigDocument(runtime.ConfigPath)
	if err != nil {
		runtime.log(r).Error("load managed rules configuration", "component", "controlapi", "event", "config_rules_load_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration could not be loaded"})
		return
	}
	info, _ := AuthInfoFromContext(r.Context())
	w.Header().Set("ETag", configETag(document.Revision))
	writeJSON(w, http.StatusOK, rulesConfigResponse{
		Revision:       document.Revision,
		Writable:       runtime.authenticator().Enabled() && info.canWrite(),
		UDPMaxSessions: document.Config.UDPMaxSessions,
		Rules:          nonNilRules(document.Config.Rules),
	})
}

func (runtime *Runtime) handleRulesConfigPrecheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !runtime.authorizeConfigWrite(w, r) {
		return
	}
	revision, err := requestRevision(r)
	if err != nil {
		writeRevisionHeaderError(w, err)
		return
	}
	draft, err := decodeRulesConfigRequest(w, r)
	if err != nil {
		writeRulesRequestError(w, err)
		return
	}

	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()
	document, candidate, result, err := runtime.prepareRulesCandidate(revision, draft)
	if err != nil {
		writeRulesCandidateError(w, err)
		return
	}
	response := rulesPrecheckResponse{
		Revision:              document.Revision,
		UDPMaxSessionsChanged: document.Config.UDPMaxSessions != candidate.Config.UDPMaxSessions,
		Diff:                  diffConfiguredRules(document.Config.Rules, candidate.Config.Rules),
		Precheck:              result,
	}
	status := http.StatusOK
	if !result.OK {
		status = http.StatusUnprocessableEntity
	}
	w.Header().Set("ETag", configETag(document.Revision))
	writeJSON(w, status, response)
}

func (runtime *Runtime) putRulesConfig(w http.ResponseWriter, r *http.Request) {
	if !runtime.authorizeConfigWrite(w, r) {
		return
	}
	revision, err := requestRevision(r)
	if err != nil {
		writeRevisionHeaderError(w, err)
		return
	}
	draft, err := decodeRulesConfigRequest(w, r)
	if err != nil {
		writeRulesRequestError(w, err)
		return
	}

	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()
	document, candidate, checkResult, err := runtime.prepareRulesCandidate(revision, draft)
	if err != nil {
		writeRulesCandidateError(w, err)
		return
	}
	if !checkResult.OK {
		writeJSON(w, http.StatusUnprocessableEntity, rulesPrecheckResponse{
			Revision:              document.Revision,
			UDPMaxSessionsChanged: document.Config.UDPMaxSessions != candidate.Config.UDPMaxSessions,
			Diff:                  diffConfiguredRules(document.Config.Rules, candidate.Config.Rules),
			Precheck:              checkResult,
		})
		return
	}

	staged, err := candidate.Stage()
	if err != nil {
		writeRulesCandidateError(w, err)
		return
	}
	defer staged.Discard()

	previousLimit, _ := runtime.Manager.UDPMaxSessions()
	if candidate.Config.UDPMaxSessions < previousLimit {
		runtime.Manager.SetUDPMaxSessions(candidate.Config.UDPMaxSessions)
	}
	pending, transaction := runtime.Manager.BeginApplySnapshotTransactional(candidate.Config.Rules, engine.ApplySnapshotOptions{ReplaceAll: true})
	if transaction.ApplyFailure != nil {
		runtime.Manager.SetUDPMaxSessions(previousLimit)
		runtime.metrics().ObserveApplyResult(transaction.Apply)
		status := http.StatusInternalServerError
		if transaction.Rollback.Failed {
			status = http.StatusServiceUnavailable
			runtime.markDegraded("rule apply rollback failed")
		}
		runtime.log(r).Error("managed rules apply failed", "component", "controlapi", "event", "config_rules_apply_failed", "rule_id", transaction.ApplyFailure.RuleID, "rollback_failed", transaction.Rollback.Failed)
		writeJSON(w, status, map[string]any{
			"error":       "configuration could not be applied",
			"transaction": transaction,
		})
		return
	}
	pendingClosed := false
	defer func() {
		if !pendingClosed {
			_ = pending.Rollback()
			runtime.Manager.SetUDPMaxSessions(previousLimit)
		}
	}()
	if runtime.configHooks != nil && runtime.configHooks.BeforeCommit != nil {
		runtime.configHooks.BeforeCommit(staged)
	}
	if err := staged.Commit(); err != nil {
		if staged.Committed() {
			runtime.Manager.SetUDPMaxSessions(candidate.Config.UDPMaxSessions)
			pending.Commit()
			pendingClosed = true
			runtime.markDegraded("configuration committed but durability sync failed")
			runtime.metrics().ObserveApplyResult(transaction.Apply)
			runtime.log(r).Error("managed rules committed with durability failure", "component", "controlapi", "event", "config_rules_commit_durability_failed", "error", err, "revision", candidate.Revision)
			w.Header().Set("ETag", configETag(candidate.Revision))
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":     "configuration committed but durability could not be confirmed",
				"committed": true,
				"revision":  candidate.Revision,
			})
			return
		}
		rollback := pending.Rollback()
		pendingClosed = true
		runtime.Manager.SetUDPMaxSessions(previousLimit)
		status := http.StatusInternalServerError
		rollbackFailed := rollback.Failed
		if rollbackFailed {
			status = http.StatusServiceUnavailable
			runtime.markDegraded("configuration commit and runtime rollback failed")
		} else if errors.Is(err, errConfigRevisionConflict) {
			status = http.StatusPreconditionFailed
		}
		runtime.log(r).Error("managed rules commit failed", "component", "controlapi", "event", "config_rules_commit_failed", "error", err, "rollback_failed", rollbackFailed)
		writeJSON(w, status, map[string]any{
			"error":       "configuration could not be committed",
			"rollback":    rollback,
			"rollback_ok": !rollbackFailed,
		})
		return
	}

	pending.Commit()
	pendingClosed = true
	runtime.Manager.SetUDPMaxSessions(candidate.Config.UDPMaxSessions)
	runtime.clearDegraded()
	runtime.metrics().ObserveApplyResult(transaction.Apply)
	runtime.log(r).Info("managed rules configuration committed", "component", "controlapi", "event", "config_rules_commit", "old_revision", document.Revision, "new_revision", candidate.Revision, "rule_count", len(candidate.Config.Rules))
	w.Header().Set("ETag", configETag(candidate.Revision))
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":         candidate.Revision,
		"writable":         true,
		"udp_max_sessions": candidate.Config.UDPMaxSessions,
		"rules":            nonNilRules(candidate.Config.Rules),
		"result":           transaction.Apply,
	})
}

func (runtime *Runtime) prepareRulesCandidate(revision string, draft rulesConfigDraft) (*configDocument, *configCandidate, precheck.Result, error) {
	document, err := loadConfigDocument(runtime.ConfigPath)
	if err != nil {
		return nil, nil, precheck.Result{}, err
	}
	if document.Revision != revision {
		return document, nil, precheck.Result{}, &configRevisionError{Expected: revision, Actual: document.Revision}
	}
	draft.Rules = prepareManagedRules(document.Config.Rules, draft.Rules, time.Now())
	candidate, err := document.BuildCandidate(draft)
	if err != nil {
		return document, nil, precheck.Result{}, &rulesValidationError{Err: err}
	}
	result := precheck.CheckConfig(candidate.Config, runtime.Manager.RunningRules(), runtime.precheckOptions())
	return document, candidate, result, nil
}

func writeRulesRequestError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body exceeds 1 MiB"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func writeRulesCandidateError(w http.ResponseWriter, err error) {
	var revisionErr *configRevisionError
	if errors.As(err, &revisionErr) {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error":            "configuration revision changed",
			"current_revision": revisionErr.Actual,
		})
		return
	}
	if errors.Is(err, errConfigRevisionConflict) {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error": "configuration revision changed",
		})
		return
	}
	var validationErr *rulesValidationError
	if errors.As(err, &validationErr) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "invalid rules configuration",
			"detail": validationErr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration operation failed"})
}

func writeRevisionHeaderError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errMissingConfigRevision) {
		status = http.StatusPreconditionRequired
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func nonNilRules(rules []engine.Rule) []engine.Rule {
	if rules == nil {
		return []engine.Rule{}
	}
	return rules
}

func (runtime *Runtime) markDegraded(cause string) {
	if runtime == nil {
		return
	}
	runtime.stateMu.Lock()
	runtime.degraded = true
	runtime.degradedCause = strings.TrimSpace(cause)
	runtime.stateMu.Unlock()
}

func (runtime *Runtime) clearDegraded() {
	if runtime == nil {
		return
	}
	runtime.stateMu.Lock()
	runtime.degraded = false
	runtime.degradedCause = ""
	runtime.stateMu.Unlock()
}

func (runtime *Runtime) degradedState() (bool, string) {
	if runtime == nil {
		return false, ""
	}
	runtime.stateMu.RLock()
	defer runtime.stateMu.RUnlock()
	return runtime.degraded, runtime.degradedCause
}

// authorizeConfigWrite deliberately requires configured authentication. The
// control API may run as root, so loopback reachability alone is not sufficient
// authorization to rewrite its configuration file.
func (runtime *Runtime) authorizeConfigWrite(w http.ResponseWriter, r *http.Request) bool {
	if !runtime.authenticator().Enabled() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "configuration management requires authentication",
		})
		runtime.log(r).Warn("configuration management denied without authentication", "component", "controlapi", "event", "config_auth_required", "method", r.Method, "path", r.URL.Path)
		return false
	}
	return runtime.authorizeWrite(w, r)
}

func decodeRulesConfigRequest(w http.ResponseWriter, r *http.Request) (rulesConfigDraft, error) {
	if r.Body == nil {
		return rulesConfigDraft{}, errors.New("missing request body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigManagementBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request rulesConfigRequest
	if err := decoder.Decode(&request); err != nil {
		return rulesConfigDraft{}, fmt.Errorf("decode request: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return rulesConfigDraft{}, err
	}
	if request.UDPMaxSessions == nil {
		return rulesConfigDraft{}, errors.New("missing udp_max_sessions")
	}
	if request.Rules == nil {
		return rulesConfigDraft{}, errors.New("missing rules")
	}
	return rulesConfigDraft{
		UDPMaxSessions: *request.UDPMaxSessions,
		Rules:          append([]engine.Rule(nil), (*request.Rules)...),
	}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON object")
		}
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return nil
}

func requestRevision(r *http.Request) (string, error) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if value == "" {
		return "", errMissingConfigRevision
	}
	if strings.HasPrefix(value, "W/") || strings.Contains(value, ",") || value == "*" {
		return "", errors.New("If-Match must contain one strong configuration revision")
	}
	if strings.HasPrefix(value, `"`) || strings.HasSuffix(value, `"`) {
		if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
			return "", errors.New("invalid If-Match configuration revision")
		}
		value = value[1 : len(value)-1]
	}
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return "", errors.New("invalid If-Match configuration revision")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil {
		return "", errors.New("invalid If-Match configuration revision")
	}
	return value, nil
}

func configETag(revision string) string {
	return `"` + strings.TrimSpace(revision) + `"`
}

func prepareManagedRules(current, desired []engine.Rule, now time.Time) []engine.Rule {
	byID := make(map[string]engine.Rule, len(current))
	for _, rule := range current {
		byID[rule.RuleID] = rule
	}
	timestamp := now.Unix()
	result := make([]engine.Rule, 0, len(desired))
	for _, incoming := range desired {
		incoming = incoming.Standardize()
		incoming.Revision = 0
		incoming.CreatedTime = 0
		incoming.UpdatedTime = 0
		old, exists := byID[incoming.RuleID]
		if !exists {
			incoming.Revision = 1
			incoming.CreatedTime = timestamp
			incoming.UpdatedTime = timestamp
			result = append(result, incoming)
			continue
		}
		if ruleConfigEqual(old, incoming) {
			incoming.Revision = old.Revision
			incoming.CreatedTime = old.CreatedTime
			incoming.UpdatedTime = old.UpdatedTime
			result = append(result, incoming)
			continue
		}
		incoming.Revision = old.Revision + 1
		if incoming.Revision < 1 {
			incoming.Revision = 1
		}
		incoming.CreatedTime = old.CreatedTime
		if incoming.CreatedTime == 0 {
			incoming.CreatedTime = timestamp
		}
		incoming.UpdatedTime = timestamp
		result = append(result, incoming)
	}
	return result
}

func ruleConfigEqual(left, right engine.Rule) bool {
	left = left.Standardize()
	right = right.Standardize()
	left.Revision, left.CreatedTime, left.UpdatedTime = 0, 0, 0
	right.Revision, right.CreatedTime, right.UpdatedTime = 0, 0, 0
	return reflect.DeepEqual(left, right)
}

func diffConfiguredRules(current, desired []engine.Rule) []configRuleDiff {
	before := make(map[string]engine.Rule, len(current))
	after := make(map[string]engine.Rule, len(desired))
	ids := make(map[string]struct{}, len(current)+len(desired))
	for _, rule := range current {
		before[rule.RuleID] = rule
		ids[rule.RuleID] = struct{}{}
	}
	for _, rule := range desired {
		after[rule.RuleID] = rule
		ids[rule.RuleID] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	diff := make([]configRuleDiff, 0, len(ordered))
	for _, id := range ordered {
		oldRule, hadOld := before[id]
		newRule, hasNew := after[id]
		item := configRuleDiff{RuleID: id}
		switch {
		case !hadOld:
			item.ConfigAction = "add"
			if newRule.Enabled {
				item.RuntimeAction = "start"
			} else {
				item.RuntimeAction = "none"
			}
		case !hasNew:
			item.ConfigAction = "delete"
			if oldRule.Enabled {
				item.RuntimeAction = "remove"
			} else {
				item.RuntimeAction = "none"
			}
		case ruleConfigEqual(oldRule, newRule):
			continue
		default:
			item.ConfigAction = "update"
			switch {
			case oldRule.Enabled && !newRule.Enabled:
				item.RuntimeAction = "stop"
			case !oldRule.Enabled && newRule.Enabled:
				item.RuntimeAction = "start"
			case oldRule.Enabled && newRule.Enabled && !oldRule.RuntimeEqual(newRule):
				item.RuntimeAction = "restart"
			default:
				item.RuntimeAction = "none"
			}
		}
		diff = append(diff, item)
	}
	return diff
}
