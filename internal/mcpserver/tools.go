package mcpserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	toolGetVMFlowStatus     = "get_vmflow_status"
	toolListForwardingRules = "list_forwarding_rules"
	toolGetForwardingRule   = "get_forwarding_rule"
	toolGetTrafficStats     = "get_traffic_stats"
	toolRunConfigPrecheck   = "run_config_precheck"

	defaultListLimit    = 50
	maxListLimit        = 200
	maxFilterLength     = 256
	toolTimeout         = 5 * time.Second
	precheckToolTimeout = 30 * time.Second
	maxConcurrentTools  = 4
)

var (
	readOnlyAnnotations = &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	}
	precheckAnnotations = &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(true),
	}
)

type emptyInput struct{}

type listForwardingRulesInput struct {
	Query    string `json:"query,omitempty" jsonschema:"case-insensitive substring filter for rule ID or name"`
	Protocol string `json:"protocol,omitempty" jsonschema:"protocol filter: tcp, udp, tcp+udp, http, or https"`
	Enabled  *bool  `json:"enabled,omitempty" jsonschema:"optional enabled-state filter"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum results to return; defaults to 50 and must not exceed 200"`
}

// forwardingRuleSummary deliberately excludes all listen/target addresses,
// source IPs, and domains. The full rule is available only through an explicit
// get_forwarding_rule call.
type forwardingRuleSummary struct {
	RuleID      string          `json:"rule_id"`
	Name        string          `json:"name"`
	Protocol    engine.Protocol `json:"protocol"`
	Enabled     bool            `json:"enabled"`
	SpeedLimit  int64           `json:"speed_limit"`
	MaxConn     int             `json:"max_conn"`
	IdleTimeout int             `json:"idle_timeout,omitempty"`
	Revision    int64           `json:"revision,omitempty"`
	UpdatedTime int64           `json:"updated_time,omitempty"`
}

type listForwardingRulesOutput struct {
	Revision string                  `json:"revision"`
	Total    int                     `json:"total"`
	Matched  int                     `json:"matched"`
	Returned int                     `json:"returned"`
	Rules    []forwardingRuleSummary `json:"rules"`
}

type getForwardingRuleInput struct {
	RuleID string `json:"rule_id" jsonschema:"exact rule ID to inspect"`
}

type getForwardingRuleOutput struct {
	Revision       string                     `json:"revision"`
	Rule           engine.Rule                `json:"rule"`
	Running        bool                       `json:"running"`
	StatsAvailable bool                       `json:"stats_available"`
	Stats          controlapi.TrafficSnapshot `json:"stats"`
}

type getTrafficStatsInput struct {
	RuleID string `json:"rule_id,omitempty" jsonschema:"optional exact rule ID filter"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum results to return; defaults to 50 and must not exceed 200"`
}

type trafficTotals struct {
	UploadBytes        int64 `json:"upload_bytes"`
	DownloadBytes      int64 `json:"download_bytes"`
	ActiveConnections  int64 `json:"active_connections"`
	SourceIPDenied     int64 `json:"source_ip_denied_total"`
	UDPSessionRejected int64 `json:"udp_session_rejected_total"`
	UDPPacketsDropped  int64 `json:"udp_packets_dropped_total"`
}

type getTrafficStatsOutput struct {
	Total    int                          `json:"total"`
	Matched  int                          `json:"matched"`
	Returned int                          `json:"returned"`
	Totals   trafficTotals                `json:"totals"`
	Items    []controlapi.TrafficSnapshot `json:"items"`
}

type runConfigPrecheckInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum findings to return; defaults to 50 and must not exceed 200"`
}

type runConfigPrecheckOutput struct {
	ConfigPath string          `json:"config_path,omitempty"`
	RuleCount  int             `json:"rule_count,omitempty"`
	Error      string          `json:"error,omitempty"`
	Result     precheck.Result `json:"result"`
	Total      int             `json:"total"`
	Returned   int             `json:"returned"`
	Truncated  bool            `json:"truncated"`
}

type statusCounters struct {
	ConfiguredRules    int   `json:"configured_rules"`
	EnabledRules       int   `json:"enabled_rules"`
	DisabledRules      int   `json:"disabled_rules"`
	RuntimeRules       int   `json:"runtime_rules"`
	TrafficRules       int   `json:"traffic_rules"`
	UploadBytes        int64 `json:"upload_bytes"`
	DownloadBytes      int64 `json:"download_bytes"`
	ActiveConnections  int64 `json:"active_connections"`
	SourceIPDenied     int64 `json:"source_ip_denied_total"`
	UDPSessionRejected int64 `json:"udp_session_rejected_total"`
	UDPPacketsDropped  int64 `json:"udp_packets_dropped_total"`
}

type getVMFlowStatusOutput struct {
	Connected            bool                           `json:"connected"`
	MCPReadOnly          bool                           `json:"mcp_read_only"`
	MCPServerVersion     string                         `json:"mcp_server_version"`
	DaemonVersion        string                         `json:"daemon_version,omitempty"`
	APIVersion           string                         `json:"api_version,omitempty"`
	Commit               string                         `json:"commit,omitempty"`
	StartedTime          int64                          `json:"started_time,omitempty"`
	Actor                string                         `json:"actor,omitempty"`
	Role                 string                         `json:"role,omitempty"`
	DaemonCapabilities   controlapi.SessionCapabilities `json:"daemon_capabilities"`
	Degraded             bool                           `json:"degraded"`
	DegradedCause        string                         `json:"degraded_cause,omitempty"`
	ConfigRevision       string                         `json:"config_revision,omitempty"`
	DaemonConfigWritable bool                           `json:"daemon_config_writable"`
	UDPMaxSessions       int                            `json:"udp_max_sessions"`
	Counters             statusCounters                 `json:"counters"`
	Issues               []string                       `json:"issues"`
}

func registerTools(server *mcp.Server, backend backend, version string) {
	handlers := newToolHandlersForBackend(backend, version)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolGetVMFlowStatus,
		Title:       "Get vmflow status",
		Description: "Report daemon connectivity, version, API compatibility, configuration health, forwarding rule counts, and aggregate traffic counters.",
		Annotations: readOnlyAnnotations,
	}, handlers.getVMFlowStatus)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolListForwardingRules,
		Title:       "List forwarding rules",
		Description: "List and filter configured forwarding rules. Summaries omit listen and target addresses, source IPs, and domains.",
		Annotations: readOnlyAnnotations,
	}, handlers.listForwardingRules)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolGetForwardingRule,
		Title:       "Get forwarding rule",
		Description: "Return the complete configuration of one explicitly selected forwarding rule, including its network endpoints and access policy.",
		Annotations: readOnlyAnnotations,
	}, handlers.getForwardingRule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolGetTrafficStats,
		Title:       "Get traffic statistics",
		Description: "Return stable, structured per-rule and aggregate traffic counters from the running daemon.",
		Annotations: readOnlyAnnotations,
	}, handlers.getTrafficStats)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolRunConfigPrecheck,
		Title:       "Run configuration precheck",
		Description: "Validate the daemon's current on-disk configuration, including listen availability and target resolution, without applying changes.",
		Annotations: precheckAnnotations,
	}, handlers.runConfigPrecheck)
}

type toolHandlers struct {
	backend backend
	version string
	gate    chan struct{}
}

func newToolHandlersForBackend(backend backend, version string) *toolHandlers {
	return &toolHandlers{
		backend: backend,
		version: version,
		gate:    make(chan struct{}, maxConcurrentTools),
	}
}

func (t *toolHandlers) getVMFlowStatus(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, getVMFlowStatusOutput, error) {
	result := getVMFlowStatusOutput{
		MCPServerVersion: t.version,
		MCPReadOnly:      true,
		Issues:           []string{},
	}
	ctx, release, err := t.acquire(ctx, toolTimeout)
	if err != nil {
		result.Issues = append(result.Issues, err.Error())
		return nil, result, nil
	}
	defer release()
	session, err := t.backend.Session(ctx)
	if err != nil {
		if status := controlapi.APIStatus(err); status != 0 {
			result.Connected = true
			switch status {
			case 401:
				result.Issues = append(result.Issues, "daemon authentication failed (HTTP 401)")
			case 403:
				result.Issues = append(result.Issues, "daemon access was denied (HTTP 403)")
			case 404:
				result.Issues = append(result.Issues, "daemon session endpoint is unavailable (HTTP 404); the daemon may be too old for this MCP server")
			default:
				result.Issues = append(result.Issues, fmt.Sprintf("daemon session request failed (HTTP %d): %v", status, err))
			}
		} else {
			result.Issues = append(result.Issues, "daemon unavailable: "+err.Error())
		}
		return nil, result, nil
	}
	if session == nil {
		result.Issues = append(result.Issues, "daemon unavailable: empty session response")
		return nil, result, nil
	}
	result.Connected = true
	result.DaemonVersion = strings.TrimSpace(session.ServerVersion)
	result.APIVersion = strings.TrimSpace(session.APIVersion)
	result.Commit = session.Commit
	result.StartedTime = session.StartedTime
	result.Actor = session.Actor
	result.Role = session.Role
	result.DaemonCapabilities = session.Capabilities
	result.Degraded = session.Degraded
	result.DegradedCause = session.DegradedCause
	if result.APIVersion == "" {
		result.Issues = append(result.Issues, "daemon API version is unavailable; the daemon may be older than this MCP server or may need to be restarted")
	} else if result.APIVersion != controlapi.ManagementAPIVersion {
		result.Issues = append(result.Issues, fmt.Sprintf("daemon API version %q is incompatible with expected version %q", result.APIVersion, controlapi.ManagementAPIVersion))
	}
	if result.DaemonVersion == "" {
		result.Issues = append(result.Issues, "daemon version is unavailable; the daemon may need to be restarted")
	}

	configRules, err := t.backend.ConfigRules(ctx)
	if err != nil {
		result.Issues = append(result.Issues, "read configured rules: "+err.Error())
	} else if configRules == nil {
		result.Issues = append(result.Issues, "read configured rules: empty response")
	} else {
		result.ConfigRevision = configRules.Revision
		result.DaemonConfigWritable = configRules.Writable
		result.UDPMaxSessions = configRules.UDPMaxSessions
		result.Counters.ConfiguredRules = len(configRules.Rules)
		for _, rule := range configRules.Rules {
			if rule.Enabled {
				result.Counters.EnabledRules++
			} else {
				result.Counters.DisabledRules++
			}
		}
	}

	runtimeRules, err := t.backend.Rules(ctx)
	if err != nil {
		result.Issues = append(result.Issues, "read runtime rules: "+err.Error())
	} else if runtimeRules == nil {
		result.Issues = append(result.Issues, "read runtime rules: empty response")
	} else {
		result.Counters.RuntimeRules = len(runtimeRules.Items)
	}

	stats, err := t.backend.Stats(ctx)
	if err != nil {
		result.Issues = append(result.Issues, "read traffic stats: "+err.Error())
	} else if stats == nil {
		result.Issues = append(result.Issues, "read traffic stats: empty response")
	} else {
		result.Counters.TrafficRules = len(stats.Items)
		totals := sumTraffic(stats.Items)
		result.Counters.UploadBytes = totals.UploadBytes
		result.Counters.DownloadBytes = totals.DownloadBytes
		result.Counters.ActiveConnections = totals.ActiveConnections
		result.Counters.SourceIPDenied = totals.SourceIPDenied
		result.Counters.UDPSessionRejected = totals.UDPSessionRejected
		result.Counters.UDPPacketsDropped = totals.UDPPacketsDropped
	}
	return nil, result, nil
}

func (t *toolHandlers) listForwardingRules(ctx context.Context, _ *mcp.CallToolRequest, input listForwardingRulesInput) (*mcp.CallToolResult, listForwardingRulesOutput, error) {
	queryValue := strings.TrimSpace(input.Query)
	if len(queryValue) > maxFilterLength {
		return nil, listForwardingRulesOutput{}, fmt.Errorf("query must not exceed %d bytes", maxFilterLength)
	}
	protocol, err := normalizeProtocolFilter(input.Protocol)
	if err != nil {
		return nil, listForwardingRulesOutput{}, err
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, listForwardingRulesOutput{}, err
	}

	ctx, release, err := t.acquire(ctx, toolTimeout)
	if err != nil {
		return nil, listForwardingRulesOutput{}, err
	}
	defer release()
	snapshot, err := t.backend.ConfigRules(ctx)
	if err != nil {
		return nil, listForwardingRulesOutput{}, fmt.Errorf("read configured rules: %w", err)
	}
	if snapshot == nil {
		return nil, listForwardingRulesOutput{}, fmt.Errorf("read configured rules: empty response")
	}

	query := strings.ToLower(queryValue)
	rules := make([]forwardingRuleSummary, 0, len(snapshot.Rules))
	for _, rule := range snapshot.Rules {
		if query != "" && !strings.Contains(strings.ToLower(rule.RuleID), query) && !strings.Contains(strings.ToLower(rule.Name), query) {
			continue
		}
		if protocol != "" && !strings.EqualFold(string(rule.Protocol), protocol) {
			continue
		}
		if input.Enabled != nil && rule.Enabled != *input.Enabled {
			continue
		}
		rules = append(rules, summarizeRule(rule))
	}
	sortRuleSummaries(rules)
	matched := len(rules)
	if len(rules) > limit {
		rules = rules[:limit]
	}
	return nil, listForwardingRulesOutput{
		Revision: snapshot.Revision,
		Total:    len(snapshot.Rules),
		Matched:  matched,
		Returned: len(rules),
		Rules:    rules,
	}, nil
}

func (t *toolHandlers) getForwardingRule(ctx context.Context, _ *mcp.CallToolRequest, input getForwardingRuleInput) (*mcp.CallToolResult, getForwardingRuleOutput, error) {
	ruleID := strings.TrimSpace(input.RuleID)
	if ruleID == "" {
		return nil, getForwardingRuleOutput{}, fmt.Errorf("rule_id is required")
	}
	if len(ruleID) > maxFilterLength {
		return nil, getForwardingRuleOutput{}, fmt.Errorf("rule_id must not exceed %d bytes", maxFilterLength)
	}
	ctx, release, err := t.acquire(ctx, toolTimeout)
	if err != nil {
		return nil, getForwardingRuleOutput{}, err
	}
	defer release()
	snapshot, err := t.backend.ConfigRules(ctx)
	if err != nil {
		return nil, getForwardingRuleOutput{}, fmt.Errorf("read configured rules: %w", err)
	}
	if snapshot == nil {
		return nil, getForwardingRuleOutput{}, fmt.Errorf("read configured rules: empty response")
	}
	for _, rule := range snapshot.Rules {
		if rule.RuleID != ruleID {
			continue
		}
		normalizeRuleCollections(&rule)
		runtimeRules, runtimeErr := t.backend.Rules(ctx)
		if runtimeErr != nil {
			return nil, getForwardingRuleOutput{}, fmt.Errorf("read runtime rules: %w", runtimeErr)
		}
		if runtimeRules == nil {
			return nil, getForwardingRuleOutput{}, fmt.Errorf("read runtime rules: empty response")
		}
		running := false
		for _, runtimeRule := range runtimeRules.Items {
			if runtimeRule.RuleID == ruleID {
				running = true
				break
			}
		}

		statsResponse, statsErr := t.backend.Stats(ctx)
		if statsErr != nil {
			return nil, getForwardingRuleOutput{}, fmt.Errorf("read traffic stats: %w", statsErr)
		}
		if statsResponse == nil {
			return nil, getForwardingRuleOutput{}, fmt.Errorf("read traffic stats: empty response")
		}
		stats := controlapi.TrafficSnapshot{RuleID: ruleID}
		statsAvailable := false
		for _, candidate := range statsResponse.Items {
			if candidate.RuleID == ruleID {
				stats = candidate
				statsAvailable = true
				break
			}
		}
		return nil, getForwardingRuleOutput{
			Revision:       snapshot.Revision,
			Rule:           rule,
			Running:        running,
			StatsAvailable: statsAvailable,
			Stats:          stats,
		}, nil
	}
	return nil, getForwardingRuleOutput{}, fmt.Errorf("forwarding rule %q was not found", ruleID)
}

func (t *toolHandlers) getTrafficStats(ctx context.Context, _ *mcp.CallToolRequest, input getTrafficStatsInput) (*mcp.CallToolResult, getTrafficStatsOutput, error) {
	ruleID := strings.TrimSpace(input.RuleID)
	if len(ruleID) > maxFilterLength {
		return nil, getTrafficStatsOutput{}, fmt.Errorf("rule_id must not exceed %d bytes", maxFilterLength)
	}
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, getTrafficStatsOutput{}, err
	}
	ctx, release, err := t.acquire(ctx, toolTimeout)
	if err != nil {
		return nil, getTrafficStatsOutput{}, err
	}
	defer release()
	stats, err := t.backend.Stats(ctx)
	if err != nil {
		return nil, getTrafficStatsOutput{}, fmt.Errorf("read traffic stats: %w", err)
	}
	if stats == nil {
		return nil, getTrafficStatsOutput{}, fmt.Errorf("read traffic stats: empty response")
	}

	items := make([]controlapi.TrafficSnapshot, 0, len(stats.Items))
	for _, item := range stats.Items {
		if ruleID == "" || item.RuleID == ruleID {
			items = append(items, item)
		}
	}
	slices.SortStableFunc(items, func(a, b controlapi.TrafficSnapshot) int {
		return cmp.Compare(a.RuleID, b.RuleID)
	})
	matched := len(items)
	totals := sumTraffic(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return nil, getTrafficStatsOutput{
		Total:    len(stats.Items),
		Matched:  matched,
		Returned: len(items),
		Totals:   totals,
		Items:    items,
	}, nil
}

func (t *toolHandlers) runConfigPrecheck(ctx context.Context, _ *mcp.CallToolRequest, input runConfigPrecheckInput) (*mcp.CallToolResult, runConfigPrecheckOutput, error) {
	limit, err := normalizeLimit(input.Limit)
	if err != nil {
		return nil, runConfigPrecheckOutput{}, err
	}
	ctx, release, err := t.acquire(ctx, precheckToolTimeout)
	if err != nil {
		return nil, runConfigPrecheckOutput{}, err
	}
	defer release()
	result, err := t.backend.CurrentPrecheck(ctx)
	if err != nil {
		return nil, runConfigPrecheckOutput{}, fmt.Errorf("run current configuration precheck: %w", err)
	}
	if result == nil {
		return nil, runConfigPrecheckOutput{}, fmt.Errorf("run current configuration precheck: empty response")
	}
	items := append([]precheck.Item(nil), result.Result.Items...)
	total := len(items)
	if len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		items = []precheck.Item{}
	}
	precheckResult := result.Result
	precheckResult.Items = items
	return nil, runConfigPrecheckOutput{
		ConfigPath: result.ConfigPath,
		RuleCount:  result.RuleCount,
		Error:      result.Error,
		Result:     precheckResult,
		Total:      total,
		Returned:   len(items),
		Truncated:  total > len(items),
	}, nil
}

func (t *toolHandlers) acquire(parent context.Context, timeout time.Duration) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	if t.gate == nil {
		return ctx, cancel, nil
	}
	select {
	case t.gate <- struct{}{}:
		return ctx, func() {
			<-t.gate
			cancel()
		}, nil
	case <-ctx.Done():
		cancel()
		return nil, nil, fmt.Errorf("wait for MCP tool capacity: %w", ctx.Err())
	}
}

func normalizeLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultListLimit, nil
	}
	if limit < 1 || limit > maxListLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxListLimit)
	}
	return limit, nil
}

func normalizeProtocolFilter(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", string(engine.ProtocolTCP), string(engine.ProtocolUDP), string(engine.ProtocolTCPUDP), string(engine.ProtocolHTTP), string(engine.ProtocolHTTPS):
		return value, nil
	default:
		return "", fmt.Errorf("protocol must be one of: tcp, udp, tcp+udp, http, https")
	}
}

func summarizeRule(rule engine.Rule) forwardingRuleSummary {
	return forwardingRuleSummary{
		RuleID:      rule.RuleID,
		Name:        rule.Name,
		Protocol:    rule.Protocol,
		Enabled:     rule.Enabled,
		SpeedLimit:  rule.SpeedLimit,
		MaxConn:     rule.MaxConn,
		IdleTimeout: rule.IdleTimeout,
		Revision:    rule.Revision,
		UpdatedTime: rule.UpdatedTime,
	}
}

func sortRuleSummaries(rules []forwardingRuleSummary) {
	slices.SortStableFunc(rules, func(a, b forwardingRuleSummary) int {
		aName := strings.ToLower(strings.TrimSpace(a.Name))
		bName := strings.ToLower(strings.TrimSpace(b.Name))
		if aName != bName {
			return cmp.Compare(aName, bName)
		}
		return cmp.Compare(a.RuleID, b.RuleID)
	})
}

func sumTraffic(items []controlapi.TrafficSnapshot) trafficTotals {
	var totals trafficTotals
	for _, item := range items {
		totals.UploadBytes += item.UploadBytes
		totals.DownloadBytes += item.DownloadBytes
		totals.ActiveConnections += item.Conns
		totals.SourceIPDenied += item.SourceIPDenied
		totals.UDPSessionRejected += item.UDPSessionRejected
		totals.UDPPacketsDropped += item.UDPPacketsDropped
	}
	return totals
}

func normalizeRuleCollections(rule *engine.Rule) {
	if rule.SourceIPs == nil {
		rule.SourceIPs = []string{}
	}
	if rule.Domains == nil {
		rule.Domains = []string{}
	}
}

func boolPtr(value bool) *bool { return &value }
