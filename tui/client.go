package tui

import (
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

// Client and API types alias controlapi so the TUI, vmflow ctl, and the
// Telegram bot share one client implementation.

type (
	Client              = controlapi.Client
	StatsResponse       = controlapi.StatsResponse
	TrafficSnapshot     = controlapi.TrafficSnapshot
	RulesResponse       = controlapi.RulesResponse
	ReloadResponse      = controlapi.ReloadResponse
	SessionCapabilities = controlapi.SessionCapabilities
	SessionResponse     = controlapi.SessionResponse
	ConfigRulesResponse = controlapi.ConfigRulesResponse
	ConfigRulesRequest  = controlapi.ConfigRulesRequest
	ConfigRuleDiff      = controlapi.ConfigRuleDiff
	PrecheckResponse    = controlapi.PrecheckResponse
	ApplyResponse       = controlapi.ApplyResponse
	BotConfigResponse   = controlapi.BotConfigResponse
	BotConfigRequest    = controlapi.BotConfigRequest
	APIError            = controlapi.APIError
)

// RuleInfo is engine.Rule, kept for TUI-internal readability.
type RuleInfo = engine.Rule

// NewClient wraps controlapi.NewClient for the TUI's variadic token signature.
func NewClient(baseURL string, token ...string) *Client {
	if len(token) > 0 {
		return controlapi.NewClient(baseURL, token[0])
	}
	return controlapi.NewClient(baseURL, "")
}

// apiStatus wraps controlapi.APIStatus.
func apiStatus(err error) int {
	return controlapi.APIStatus(err)
}

// cloneRules wraps controlapi.CloneRules.
func cloneRules(rules []RuleInfo) []RuleInfo {
	return controlapi.CloneRules(rules)
}

// normalizeETag wraps controlapi.NormalizeETag.
func normalizeETag(value string) string {
	return controlapi.NormalizeETag(value)
}
