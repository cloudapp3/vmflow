package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	toolUpsertForwardingRule     = "upsert_forwarding_rule"
	toolDeleteForwardingRule     = "delete_forwarding_rule"
	toolSetForwardingRuleEnabled = "set_forwarding_rule_enabled"

	defaultListenAddr = "127.0.0.1"
	maxRuleIDLength   = 64
)

var (
	writeAnnotations = &mcp.ToolAnnotations{
		ReadOnlyHint:   false,
		OpenWorldHint:  boolPtr(false),
		IdempotentHint: true,
	}
	deleteAnnotations = &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		OpenWorldHint:   boolPtr(false),
		IdempotentHint:  true,
		DestructiveHint: boolPtr(true),
	}
)

// bandwidthLimitInput mirrors engine.BandwidthLimit with string quantities so
// clients send human-readable values such as "100 Mbps". Quantities parse
// through engine's own unit tables.
type bandwidthLimitInput struct {
	Rate     string `json:"rate,omitempty" jsonschema:"symmetric per-direction limit such as \"100 Mbps\" or \"12 MiB/s\"; cannot be combined with upload or download"`
	Upload   string `json:"upload,omitempty" jsonschema:"upload-only limit such as \"50 Mbps\"; cannot be combined with rate"`
	Download string `json:"download,omitempty" jsonschema:"download-only limit such as \"200 Mbps\"; cannot be combined with rate"`
}

// trafficQuotaInput mirrors engine.TrafficQuota with string quantities.
type trafficQuotaInput struct {
	Daily    string `json:"daily,omitempty" jsonschema:"calendar-day upload+download quota such as \"50GB\"; daily or monthly is required"`
	Monthly  string `json:"monthly,omitempty" jsonschema:"calendar-month quota such as \"1TB\""`
	Timezone string `json:"timezone,omitempty" jsonschema:"IANA timezone for quota windows; defaults to UTC"`
}

type upsertForwardingRuleInput struct {
	RuleID           string               `json:"rule_id" jsonschema:"unique rule ID; upserting an existing rule keeps its identity but replaces its configuration"`
	Name             string               `json:"name" jsonschema:"human-readable rule name"`
	Protocol         string               `json:"protocol" jsonschema:"protocol: tcp, udp, or tcp+udp"`
	ListenAddr       string               `json:"listen_addr,omitempty" jsonschema:"listen address; defaults to 127.0.0.1; a non-loopback address requires allow_non_loopback=true"`
	ListenPort       int                  `json:"listen_port" jsonschema:"listen port between 1 and 65535"`
	TargetAddr       string               `json:"target_addr" jsonschema:"target address to forward traffic to"`
	TargetPort       int                  `json:"target_port" jsonschema:"target port between 1 and 65535"`
	Enabled          *bool                `json:"enabled,omitempty" jsonschema:"whether the rule runs; defaults to true"`
	SpeedLimit       int64                `json:"speed_limit,omitempty" jsonschema:"legacy per-connection limit in bytes per second; 0 means unlimited; prefer bandwidth_limit"`
	Bandwidth        *bandwidthLimitInput `json:"bandwidth_limit,omitempty" jsonschema:"aggregate whole-rule bandwidth policy"`
	TrafficQuota     *trafficQuotaInput   `json:"traffic_quota,omitempty" jsonschema:"durable daily and monthly traffic quota"`
	MaxConn          int                  `json:"max_conn,omitempty" jsonschema:"maximum concurrent TCP connections; 0 means unlimited"`
	IdleTimeout      int                  `json:"idle_timeout,omitempty" jsonschema:"idle timeout in seconds; 0 uses the daemon default"`
	SourceIPMode     string               `json:"source_ip_mode,omitempty" jsonschema:"source policy: allowlist or denylist; required when source_ips is set"`
	SourceIPs        []string             `json:"source_ips,omitempty" jsonschema:"IPv4/IPv6 addresses or CIDRs matched by source_ip_mode"`
	Domains          []string             `json:"domains,omitempty" jsonschema:"domains bound to this rule (currently only used by https rules)"`
	Remark           string               `json:"remark,omitempty" jsonschema:"free-form note stored with the rule"`
	AllowNonLoopback bool                 `json:"allow_non_loopback,omitempty" jsonschema:"must be true to accept a listen address reachable from other hosts"`
	DryRun           bool                 `json:"dry_run,omitempty" jsonschema:"validate the change through daemon precheck without applying it"`
}

type deleteForwardingRuleInput struct {
	RuleID string `json:"rule_id" jsonschema:"ID of the rule to delete"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"validate the deletion through daemon precheck without applying it"`
}

type setForwardingRuleEnabledInput struct {
	RuleID  string `json:"rule_id" jsonschema:"ID of the rule to enable or disable"`
	Enabled bool   `json:"enabled" jsonschema:"true starts the rule, false stops it"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"validate the change through daemon precheck without applying it"`
}

// mutateForwardingRuleOutput is the shared result of one write-tool call.
type mutateForwardingRuleOutput struct {
	Action      string                      `json:"action"`
	Applied     bool                        `json:"applied"`
	DryRun      bool                        `json:"dry_run"`
	Revision    string                      `json:"revision,omitempty"`
	Rule        *forwardingRuleView         `json:"rule,omitempty"`
	Diff        []controlapi.ConfigRuleDiff `json:"diff,omitempty"`
	Precheck    *precheck.Result            `json:"precheck,omitempty"`
	ApplyResult *engine.ApplyResult         `json:"apply_result,omitempty"`
	Issues      []string                    `json:"issues"`
}

func registerWriteTools(server *mcp.Server, handlers *toolHandlers) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolUpsertForwardingRule,
		Title:       "Create or update a forwarding rule",
		Description: "Create one forwarding rule or replace the configuration of an existing rule by rule_id. Other rules are left untouched. The input is the complete desired rule state, so inspect the current rule with get_forwarding_rule before modifying it. Set dry_run=true to validate without applying.",
		Annotations: writeAnnotations,
	}, handlers.upsertForwardingRule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolDeleteForwardingRule,
		Title:       "Delete a forwarding rule",
		Description: "Remove one forwarding rule by rule_id and stop its listeners. Other rules are left untouched. Set dry_run=true to validate without applying.",
		Annotations: deleteAnnotations,
	}, handlers.deleteForwardingRule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolSetForwardingRuleEnabled,
		Title:       "Start or stop a forwarding rule",
		Description: "Enable or disable one forwarding rule by rule_id without changing its configuration. Set dry_run=true to validate without applying.",
		Annotations: writeAnnotations,
	}, handlers.setForwardingRuleEnabled)
}

func (t *toolHandlers) upsertForwardingRule(ctx context.Context, _ *mcp.CallToolRequest, input upsertForwardingRuleInput) (*mcp.CallToolResult, mutateForwardingRuleOutput, error) {
	rule, err := buildUpsertRule(input)
	if err != nil {
		return nil, mutateForwardingRuleOutput{}, err
	}
	return t.mutateRule(ctx, rule.RuleID, input.DryRun, "updated", func(rules []engine.Rule) ([]engine.Rule, bool, *engine.Rule, error) {
		for index := range rules {
			if rules[index].RuleID != rule.RuleID {
				continue
			}
			// Preserve server-managed identity fields; the daemon bumps
			// revision and updated_time on commit.
			rule.CreatedTime = rules[index].CreatedTime
			rule.Revision = rules[index].Revision
			rules[index] = rule
			return rules, false, &rule, nil
		}
		rules = append(rules, rule)
		return rules, true, &rule, nil
	})
}

func (t *toolHandlers) deleteForwardingRule(ctx context.Context, _ *mcp.CallToolRequest, input deleteForwardingRuleInput) (*mcp.CallToolResult, mutateForwardingRuleOutput, error) {
	ruleID, err := normalizeRuleID(input.RuleID)
	if err != nil {
		return nil, mutateForwardingRuleOutput{}, err
	}
	return t.mutateRule(ctx, ruleID, input.DryRun, "deleted", func(rules []engine.Rule) ([]engine.Rule, bool, *engine.Rule, error) {
		for index := range rules {
			if rules[index].RuleID != ruleID {
				continue
			}
			removed := rules[index]
			rules = append(rules[:index], rules[index+1:]...)
			return rules, false, &removed, nil
		}
		return nil, false, nil, fmt.Errorf("forwarding rule %q was not found", ruleID)
	})
}

func (t *toolHandlers) setForwardingRuleEnabled(ctx context.Context, _ *mcp.CallToolRequest, input setForwardingRuleEnabledInput) (*mcp.CallToolResult, mutateForwardingRuleOutput, error) {
	ruleID, err := normalizeRuleID(input.RuleID)
	if err != nil {
		return nil, mutateForwardingRuleOutput{}, err
	}
	action := "disabled"
	if input.Enabled {
		action = "enabled"
	}
	return t.mutateRule(ctx, ruleID, input.DryRun, action, func(rules []engine.Rule) ([]engine.Rule, bool, *engine.Rule, error) {
		for index := range rules {
			if rules[index].RuleID != ruleID {
				continue
			}
			if rules[index].Enabled == input.Enabled {
				current := rules[index]
				return nil, false, &current, errRuleUnchanged
			}
			rules[index].Enabled = input.Enabled
			updated := rules[index]
			return rules, false, &updated, nil
		}
		return nil, false, nil, fmt.Errorf("forwarding rule %q was not found", ruleID)
	})
}

// errRuleUnchanged signals a no-op mutation (for example enabling a rule that
// is already enabled); mutateRule reports it without contacting the daemon.
var errRuleUnchanged = errors.New("forwarding rule is already in the requested state")

// mutateRule reads the current snapshot, applies one single-rule mutation,
// and either prechecks or applies the resulting draft with optimistic
// concurrency. The mutation callback returns the new rules slice, whether the
// rule was newly created, the resulting rule (nil when deleted), and an error
// for request-level failures such as an unknown rule ID.
func (t *toolHandlers) mutateRule(ctx context.Context, rawRuleID string, dryRun bool, action string, mutate func(rules []engine.Rule) ([]engine.Rule, bool, *engine.Rule, error)) (*mcp.CallToolResult, mutateForwardingRuleOutput, error) {
	out := mutateForwardingRuleOutput{Action: action, DryRun: dryRun, Issues: []string{}}
	if t.write == nil {
		return nil, out, writeDisabled()
	}
	ruleID, err := normalizeRuleID(rawRuleID)
	if err != nil {
		return nil, out, err
	}

	ctx, release, err := t.acquire(ctx, precheckToolTimeout)
	if err != nil {
		return nil, out, err
	}
	defer release()

	snapshot, err := t.backend.ConfigRules(ctx)
	if err != nil {
		return nil, out, fmt.Errorf("read configured rules: %w", err)
	}
	if snapshot == nil {
		return nil, out, fmt.Errorf("read configured rules: empty response")
	}
	if !snapshot.Writable {
		return nil, out, fmt.Errorf("this session cannot write the daemon configuration; when authentication is enabled, use an admin management token (get_vmflow_status reports the session role)")
	}

	rules, created, resultRule, err := mutate(append([]engine.Rule(nil), snapshot.Rules...))
	if errors.Is(err, errRuleUnchanged) {
		out.Action = "unchanged"
		out.Rule = ruleViewPtr(resultRule)
		out.Issues = append(out.Issues, errRuleUnchanged.Error())
		return nil, out, nil
	}
	if err != nil {
		return nil, out, err
	}
	if created {
		out.Action = "created"
	}
	out.Rule = ruleViewPtr(resultRule)

	draft := controlapi.ConfigRulesRequest{UDPMaxSessions: snapshot.UDPMaxSessions, Rules: rules}
	if dryRun {
		precheckResponse, err := t.write.Precheck(ctx, snapshot.ETag, draft)
		if err != nil {
			return nil, out, mapWriteError(err)
		}
		if precheckResponse == nil {
			return nil, out, fmt.Errorf("precheck draft: empty response")
		}
		result := precheckResponse.Precheck
		out.Precheck = &result
		out.Diff = precheckResponse.Diff
		out.Revision = snapshot.Revision
		if !result.OK {
			out.Issues = append(out.Issues, fmt.Sprintf("precheck reported %d error(s) and %d warning(s)", result.ErrorCount, result.WarningCount))
		}
		return nil, out, nil
	}

	applyResponse, err := t.write.Apply(ctx, snapshot.ETag, draft)
	if err != nil {
		return nil, out, mapWriteError(err)
	}
	if applyResponse == nil {
		return nil, out, fmt.Errorf("apply draft: empty response")
	}
	out.Applied = true
	out.Revision = applyResponse.Revision
	for index := range applyResponse.Rules {
		if applyResponse.Rules[index].RuleID == ruleID {
			normalized := applyResponse.Rules[index]
			out.Rule = ruleViewPtr(&normalized)
			break
		}
	}
	applyResult := applyResponse.Result
	out.ApplyResult = &applyResult
	return nil, out, nil
}

func buildUpsertRule(input upsertForwardingRuleInput) (engine.Rule, error) {
	ruleID, err := normalizeRuleID(input.RuleID)
	if err != nil {
		return engine.Rule{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return engine.Rule{}, fmt.Errorf("name is required")
	}
	if len(name) > maxFilterLength {
		return engine.Rule{}, fmt.Errorf("name must not exceed %d bytes", maxFilterLength)
	}
	protocol, err := normalizeWritableProtocol(input.Protocol)
	if err != nil {
		return engine.Rule{}, err
	}
	listenAddr := strings.TrimSpace(input.ListenAddr)
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	if !isLoopbackListenAddr(listenAddr) && !input.AllowNonLoopback {
		return engine.Rule{}, fmt.Errorf("listen_addr %q is not loopback; forwarding other hosts' traffic requires allow_non_loopback=true", listenAddr)
	}
	if input.ListenPort < 1 || input.ListenPort > 65535 {
		return engine.Rule{}, fmt.Errorf("listen_port must be between 1 and 65535")
	}
	targetAddr := strings.TrimSpace(input.TargetAddr)
	if targetAddr == "" {
		return engine.Rule{}, fmt.Errorf("target_addr is required")
	}
	if input.TargetPort < 1 || input.TargetPort > 65535 {
		return engine.Rule{}, fmt.Errorf("target_port must be between 1 and 65535")
	}

	rule := engine.Rule{
		RuleID:       ruleID,
		Name:         name,
		Protocol:     protocol,
		ListenAddr:   listenAddr,
		ListenPort:   input.ListenPort,
		TargetAddr:   targetAddr,
		TargetPort:   input.TargetPort,
		Enabled:      true,
		SpeedLimit:   input.SpeedLimit,
		MaxConn:      input.MaxConn,
		IdleTimeout:  input.IdleTimeout,
		SourceIPMode: engine.SourceIPMode(strings.TrimSpace(input.SourceIPMode)),
		SourceIPs:    append([]string(nil), input.SourceIPs...),
		Domains:      append([]string(nil), input.Domains...),
		Remark:       strings.TrimSpace(input.Remark),
	}
	if input.Enabled != nil {
		rule.Enabled = *input.Enabled
	}
	if input.Bandwidth != nil {
		bandwidth, err := parseBandwidthLimit(*input.Bandwidth)
		if err != nil {
			return engine.Rule{}, err
		}
		rule.Bandwidth = bandwidth
	}
	if input.TrafficQuota != nil {
		quota, err := parseTrafficQuota(*input.TrafficQuota)
		if err != nil {
			return engine.Rule{}, err
		}
		rule.TrafficQuota = quota
	}

	rule = rule.Standardize()
	if err := rule.Validate(); err != nil {
		return engine.Rule{}, fmt.Errorf("invalid rule: %w", err)
	}
	return rule, nil
}

func parseBandwidthLimit(input bandwidthLimitInput) (*engine.BandwidthLimit, error) {
	limit := &engine.BandwidthLimit{}
	if rate, err := parseRateField(input.Rate, "bandwidth_limit.rate"); err != nil {
		return nil, err
	} else if rate != nil {
		limit.Rate = *rate
	}
	if upload, err := parseRateField(input.Upload, "bandwidth_limit.upload"); err != nil {
		return nil, err
	} else if upload != nil {
		limit.Upload = *upload
	}
	if download, err := parseRateField(input.Download, "bandwidth_limit.download"); err != nil {
		return nil, err
	} else if download != nil {
		limit.Download = *download
	}
	return limit, nil
}

func parseTrafficQuota(input trafficQuotaInput) (*engine.TrafficQuota, error) {
	quota := &engine.TrafficQuota{Timezone: strings.TrimSpace(input.Timezone)}
	if daily, err := parseSizeField(input.Daily, "traffic_quota.daily"); err != nil {
		return nil, err
	} else if daily != nil {
		quota.Daily = *daily
	}
	if monthly, err := parseSizeField(input.Monthly, "traffic_quota.monthly"); err != nil {
		return nil, err
	} else if monthly != nil {
		quota.Monthly = *monthly
	}
	quota.Action = "block"
	return quota, nil
}

func parseRateField(value, field string) (*engine.ByteRate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var rate engine.ByteRate
	if err := rate.UnmarshalText([]byte(value)); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return &rate, nil
}

func parseSizeField(value, field string) (*engine.ByteSize, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var size engine.ByteSize
	if err := size.UnmarshalText([]byte(value)); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return &size, nil
}

func normalizeRuleID(raw string) (string, error) {
	ruleID := strings.TrimSpace(raw)
	if ruleID == "" {
		return "", fmt.Errorf("rule_id is required")
	}
	if len(ruleID) > maxRuleIDLength {
		return "", fmt.Errorf("rule_id must not exceed %d bytes", maxRuleIDLength)
	}
	return ruleID, nil
}

func normalizeWritableProtocol(value string) (engine.Protocol, error) {
	protocol := engine.Protocol(strings.ToLower(strings.TrimSpace(value)))
	switch protocol {
	case engine.ProtocolTCP, engine.ProtocolUDP, engine.ProtocolTCPUDP:
		return protocol, nil
	default:
		return "", fmt.Errorf("protocol must be one of: tcp, udp, tcp+udp")
	}
}

// isLoopbackListenAddr reports whether addr limits listeners to this host. An
// empty address, 0.0.0.0, and :: listen on every interface, so they count as
// non-loopback exposure.
func isLoopbackListenAddr(addr string) bool {
	host := strings.TrimSpace(addr)
	if host == "" {
		return false
	}
	if withoutPort, _, err := net.SplitHostPort(host); err == nil {
		host = withoutPort
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// mapWriteError converts daemon write failures into model-actionable errors.
func mapWriteError(err error) error {
	switch controlapi.APIStatus(err) {
	case http.StatusPreconditionFailed:
		return fmt.Errorf("the configuration was modified concurrently; re-read it with list_forwarding_rules and retry: %w", err)
	case http.StatusForbidden:
		return fmt.Errorf("the daemon denied the write; an admin management token is required: %w", err)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("the draft failed precheck validation (port conflicts or invalid configuration): %w", err)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("the daemon rejected the apply and rolled back its running state; check daemon logs: %w", err)
	default:
		return fmt.Errorf("submit rule change: %w", err)
	}
}
