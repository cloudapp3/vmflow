package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudapp3/vmflow/engine"
)

// Collector renders vmflow metrics in Prometheus text exposition format.
type Collector struct {
	manager *engine.Manager
	started time.Time

	mu            sync.RWMutex
	adminRequests map[adminRequestKey]*adminRequestStats
	reloads       map[string]int64
	applyActions  map[applyActionKey]int64
}

type adminRequestKey struct {
	Method string
	Path   string
	Status string
}

type adminRequestStats struct {
	Count       int64
	DurationSum float64
}

type applyActionKey struct {
	Action string
	Status string
}

// New creates a metrics collector backed by an engine manager.
func New(manager *engine.Manager) *Collector {
	return &Collector{
		manager:       manager,
		started:       time.Now(),
		adminRequests: make(map[adminRequestKey]*adminRequestStats),
		reloads:       make(map[string]int64),
		applyActions:  make(map[applyActionKey]int64),
	}
}

// ObserveAdminRequest records one Admin API request.
func (c *Collector) ObserveAdminRequest(method, path string, status int, duration time.Duration) {
	if c == nil {
		return
	}
	key := adminRequestKey{
		Method: strings.ToUpper(strings.TrimSpace(method)),
		Path:   normalizePath(path),
		Status: strconv.Itoa(status),
	}
	c.mu.Lock()
	stats := c.adminRequests[key]
	if stats == nil {
		stats = &adminRequestStats{}
		c.adminRequests[key] = stats
	}
	stats.Count++
	stats.DurationSum += duration.Seconds()
	c.mu.Unlock()
}

// ObserveReload records one reload attempt.
func (c *Collector) ObserveReload(status string) {
	if c == nil {
		return
	}
	status = normalizeStatus(status)
	c.mu.Lock()
	c.reloads[status]++
	c.mu.Unlock()
}

// ObserveApplyResult records rule apply actions from a snapshot operation.
func (c *Collector) ObserveApplyResult(result engine.ApplyResult) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for _, item := range result.Items {
		key := applyActionKey{Action: string(item.Action), Status: normalizeStatus(item.Status)}
		c.applyActions[key]++
	}
	c.mu.Unlock()
}

// Handler returns an HTTP handler for /metrics.
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte("method not allowed\n"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = c.Write(w)
	})
}

// Write renders metrics to writer.
func (c *Collector) Write(w io.Writer) error {
	if c == nil {
		return nil
	}
	rules := c.runningRules()
	snapshots := c.snapshots()
	adminRequests, reloads, applyActions := c.copyCounters()

	if _, err := fmt.Fprintf(w, "# HELP vmflow_build_info Static build info for vmflow.\n# TYPE vmflow_build_info gauge\nvmflow_build_info 1\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# HELP vmflow_uptime_seconds Time since this vmflow process metrics collector started.\n# TYPE vmflow_uptime_seconds gauge\nvmflow_uptime_seconds %.0f\n", time.Since(c.started).Seconds()); err != nil {
		return err
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_running Whether a forwarding rule is currently running.\n# TYPE vmflow_rule_running gauge\n"); err != nil {
		return err
	}
	for _, rule := range rules {
		if _, err := fmt.Fprintf(w, "vmflow_rule_running{rule_id=%q,protocol=%q} 1\n", rule.RuleID, string(rule.Protocol)); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_connections Current active connections or UDP sessions by rule.\n# TYPE vmflow_rule_connections gauge\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(rules, sample.RuleID)
		if _, err := fmt.Fprintf(w, "vmflow_rule_connections{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.Conns); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_upload_bytes_total Total uploaded bytes by rule.\n# TYPE vmflow_rule_upload_bytes_total counter\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(rules, sample.RuleID)
		if _, err := fmt.Fprintf(w, "vmflow_rule_upload_bytes_total{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.UploadBytes); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_download_bytes_total Total downloaded bytes by rule.\n# TYPE vmflow_rule_download_bytes_total counter\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(rules, sample.RuleID)
		if _, err := fmt.Fprintf(w, "vmflow_rule_download_bytes_total{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.DownloadBytes); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_admin_requests_total Total Admin API requests.\n# TYPE vmflow_admin_requests_total counter\n"); err != nil {
		return err
	}
	for _, key := range sortedAdminKeys(adminRequests) {
		stats := adminRequests[key]
		if _, err := fmt.Fprintf(w, "vmflow_admin_requests_total{method=%q,path=%q,status=%q} %d\n", key.Method, key.Path, key.Status, stats.Count); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_admin_request_duration_seconds_sum Total Admin API request duration in seconds.\n# TYPE vmflow_admin_request_duration_seconds_sum counter\n"); err != nil {
		return err
	}
	for _, key := range sortedAdminKeys(adminRequests) {
		stats := adminRequests[key]
		if _, err := fmt.Fprintf(w, "vmflow_admin_request_duration_seconds_sum{method=%q,path=%q,status=%q} %.6f\n", key.Method, key.Path, key.Status, stats.DurationSum); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_reload_total Total reload attempts by status.\n# TYPE vmflow_reload_total counter\n"); err != nil {
		return err
	}
	for _, status := range sortedStringKeys(reloads) {
		if _, err := fmt.Fprintf(w, "vmflow_reload_total{status=%q} %d\n", status, reloads[status]); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_apply_total Total rule apply actions by action and status.\n# TYPE vmflow_rule_apply_total counter\n"); err != nil {
		return err
	}
	for _, key := range sortedApplyKeys(applyActions) {
		if _, err := fmt.Fprintf(w, "vmflow_rule_apply_total{action=%q,status=%q} %d\n", key.Action, key.Status, applyActions[key]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) runningRules() []engine.Rule {
	if c == nil || c.manager == nil {
		return nil
	}
	return c.manager.RunningRules()
}

func (c *Collector) snapshots() []engine.TrafficSnapshot {
	if c == nil || c.manager == nil {
		return nil
	}
	return c.manager.SnapshotAll()
}

func (c *Collector) copyCounters() (map[adminRequestKey]adminRequestStats, map[string]int64, map[applyActionKey]int64) {
	adminRequests := make(map[adminRequestKey]adminRequestStats)
	reloads := make(map[string]int64)
	applyActions := make(map[applyActionKey]int64)

	if c == nil {
		return adminRequests, reloads, applyActions
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, stats := range c.adminRequests {
		if stats != nil {
			adminRequests[key] = *stats
		}
	}
	for key, value := range c.reloads {
		reloads[key] = value
	}
	for key, value := range c.applyActions {
		applyActions[key] = value
	}
	return adminRequests, reloads, applyActions
}

func protocolForRule(rules []engine.Rule, ruleID string) string {
	for _, rule := range rules {
		if rule.RuleID == ruleID {
			return string(rule.Protocol)
		}
	}
	return "unknown"
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	return path
}

func normalizeStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "unknown"
	}
	return status
}

func sortedStringKeys(values map[string]int64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedAdminKeys(values map[adminRequestKey]adminRequestStats) []adminRequestKey {
	keys := make([]adminRequestKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Path != keys[j].Path {
			return keys[i].Path < keys[j].Path
		}
		if keys[i].Method != keys[j].Method {
			return keys[i].Method < keys[j].Method
		}
		return keys[i].Status < keys[j].Status
	})
	return keys
}

func sortedApplyKeys(values map[applyActionKey]int64) []applyActionKey {
	keys := make([]applyActionKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Action != keys[j].Action {
			return keys[i].Action < keys[j].Action
		}
		return keys[i].Status < keys[j].Status
	})
	return keys
}
