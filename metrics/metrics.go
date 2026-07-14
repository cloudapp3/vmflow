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

	mu              sync.RWMutex
	controlRequests map[controlRequestKey]*controlRequestStats
	reloads         map[string]int64
	applyActions    map[applyActionKey]int64
}

type controlRequestKey struct {
	Method string
	Path   string
	Status string
}

type controlRequestStats struct {
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
		manager:         manager,
		started:         time.Now(),
		controlRequests: make(map[controlRequestKey]*controlRequestStats),
		reloads:         make(map[string]int64),
		applyActions:    make(map[applyActionKey]int64),
	}
}

// ObserveControlRequest records one Control API request.
func (c *Collector) ObserveControlRequest(method, path string, status int, duration time.Duration) {
	if c == nil {
		return
	}
	key := controlRequestKey{
		Method: normalizeMethod(method),
		Path:   normalizeControlPath(path),
		Status: strconv.Itoa(status),
	}
	c.mu.Lock()
	stats := c.controlRequests[key]
	if stats == nil {
		stats = &controlRequestStats{}
		c.controlRequests[key] = stats
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
	protocols := c.ruleProtocols()
	controlRequests, reloads, applyActions := c.copyCounters()

	if _, err := fmt.Fprintf(w, "# HELP vmflow_build_info Static build info for vmflow.\n# TYPE vmflow_build_info gauge\nvmflow_build_info 1\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# HELP vmflow_uptime_seconds Time since this vmflow process metrics collector started.\n# TYPE vmflow_uptime_seconds gauge\nvmflow_uptime_seconds %.0f\n", time.Since(c.started).Seconds()); err != nil {
		return err
	}
	udpLimit, udpActive := engine.DefaultUDPGlobalMaxSessions, 0
	if c.manager != nil {
		udpLimit, udpActive = c.manager.UDPMaxSessions()
	}
	if _, err := fmt.Fprintf(w, "# HELP vmflow_udp_sessions_limit Manager-wide UDP session admission limit.\n# TYPE vmflow_udp_sessions_limit gauge\nvmflow_udp_sessions_limit %d\n# HELP vmflow_udp_sessions_active Active UDP sessions across all rules in this manager.\n# TYPE vmflow_udp_sessions_active gauge\nvmflow_udp_sessions_active %d\n", udpLimit, udpActive); err != nil {
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
		protocol := protocolForRule(protocols, sample.RuleID)
		if _, err := fmt.Fprintf(w, "vmflow_rule_connections{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.Conns); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_upload_bytes_total Total uploaded bytes by rule.\n# TYPE vmflow_rule_upload_bytes_total counter\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(protocols, sample.RuleID)
		if _, err := fmt.Fprintf(w, "vmflow_rule_upload_bytes_total{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.UploadBytes); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_rule_download_bytes_total Total downloaded bytes by rule.\n# TYPE vmflow_rule_download_bytes_total counter\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(protocols, sample.RuleID)
		if _, err := fmt.Fprintf(w, "vmflow_rule_download_bytes_total{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.DownloadBytes); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_udp_session_rejected_total Total UDP session admission attempts rejected by rule or manager limits.\n# TYPE vmflow_udp_session_rejected_total counter\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(protocols, sample.RuleID)
		if !hasUDPStats(protocol, sample) {
			continue
		}
		if _, err := fmt.Fprintf(w, "vmflow_udp_session_rejected_total{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.UDPSessionRejected); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_udp_packets_dropped_total Total UDP packets dropped because the bounded upload queue was full.\n# TYPE vmflow_udp_packets_dropped_total counter\n"); err != nil {
		return err
	}
	for _, sample := range snapshots {
		protocol := protocolForRule(protocols, sample.RuleID)
		if !hasUDPStats(protocol, sample) {
			continue
		}
		if _, err := fmt.Fprintf(w, "vmflow_udp_packets_dropped_total{rule_id=%q,protocol=%q} %d\n", sample.RuleID, protocol, sample.UDPPacketsDropped); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_control_requests_total Total Control API requests.\n# TYPE vmflow_control_requests_total counter\n"); err != nil {
		return err
	}
	for _, key := range sortedControlKeys(controlRequests) {
		stats := controlRequests[key]
		if _, err := fmt.Fprintf(w, "vmflow_control_requests_total{method=%q,path=%q,status=%q} %d\n", key.Method, key.Path, key.Status, stats.Count); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprint(w, "# HELP vmflow_control_request_duration_seconds_sum Total Control API request duration in seconds.\n# TYPE vmflow_control_request_duration_seconds_sum counter\n"); err != nil {
		return err
	}
	for _, key := range sortedControlKeys(controlRequests) {
		stats := controlRequests[key]
		if _, err := fmt.Fprintf(w, "vmflow_control_request_duration_seconds_sum{method=%q,path=%q,status=%q} %.6f\n", key.Method, key.Path, key.Status, stats.DurationSum); err != nil {
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

func (c *Collector) ruleProtocols() map[string]engine.Protocol {
	if c == nil || c.manager == nil {
		return nil
	}
	return c.manager.RuleProtocols()
}

func (c *Collector) snapshots() []engine.TrafficSnapshot {
	if c == nil || c.manager == nil {
		return nil
	}
	return c.manager.SnapshotAll()
}

func (c *Collector) copyCounters() (map[controlRequestKey]controlRequestStats, map[string]int64, map[applyActionKey]int64) {
	controlRequests := make(map[controlRequestKey]controlRequestStats)
	reloads := make(map[string]int64)
	applyActions := make(map[applyActionKey]int64)

	if c == nil {
		return controlRequests, reloads, applyActions
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, stats := range c.controlRequests {
		if stats != nil {
			controlRequests[key] = *stats
		}
	}
	for key, value := range c.reloads {
		reloads[key] = value
	}
	for key, value := range c.applyActions {
		applyActions[key] = value
	}
	return controlRequests, reloads, applyActions
}

func protocolForRule(protocols map[string]engine.Protocol, ruleID string) string {
	if protocol := protocols[ruleID]; protocol != "" {
		return string(protocol)
	}
	return "unknown"
}

func indexRuleProtocols(rules []engine.Rule) map[string]engine.Protocol {
	protocols := make(map[string]engine.Protocol, len(rules))
	for _, rule := range rules {
		protocols[rule.RuleID] = rule.Protocol
	}
	return protocols
}

func hasUDPStats(protocol string, sample engine.TrafficSnapshot) bool {
	return protocol == string(engine.ProtocolUDP) ||
		protocol == string(engine.ProtocolTCPUDP) ||
		sample.UDPSessionRejected != 0 ||
		sample.UDPPacketsDropped != 0
}

func normalizeMethod(method string) string {
	switch method = strings.ToUpper(strings.TrimSpace(method)); method {
	case http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions:
		return method
	default:
		return "OTHER"
	}
}

func normalizeControlPath(path string) string {
	path = strings.TrimSpace(path)
	switch path {
	case "/metrics",
		"/v1/rules",
		"/v1/stats",
		"/v1/precheck",
		"/v1/reload",
		"/v1/certs",
		"/v1/certs/obtain",
		"/v1/certs/review":
		return path
	}

	const certDetailPrefix = "/v1/certs/"
	if suffix, ok := strings.CutPrefix(path, certDetailPrefix); ok && suffix != "" && !strings.Contains(suffix, "/") {
		return "/v1/certs/{domain}"
	}
	return "/unknown"
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

func sortedControlKeys(values map[controlRequestKey]controlRequestStats) []controlRequestKey {
	keys := make([]controlRequestKey, 0, len(values))
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
