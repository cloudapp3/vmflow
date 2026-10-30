package mcpserver

import (
	"github.com/cloudapp3/vmflow/engine"
)

// bandwidthLimitView and trafficQuotaView expose rule policies with
// human-readable string quantities. engine.ByteRate and engine.ByteSize
// marshal as strings ("100 Mbps") but reflect as integers, so embedding them
// in tool output makes the SDK's output-schema validation fail. String views
// keep the advertised schema and the marshaled JSON in agreement.
type bandwidthLimitView struct {
	Rate     string `json:"rate,omitempty"`
	Upload   string `json:"upload,omitempty"`
	Download string `json:"download,omitempty"`
}

type trafficQuotaView struct {
	Daily    string `json:"daily,omitempty"`
	Monthly  string `json:"monthly,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Action   string `json:"action,omitempty"`
}

// forwardingRuleView mirrors engine.Rule with schema-accurate policy views.
type forwardingRuleView struct {
	RuleID       string              `json:"rule_id"`
	Name         string              `json:"name"`
	Protocol     engine.Protocol     `json:"protocol"`
	ListenAddr   string              `json:"listen_addr"`
	ListenPort   int                 `json:"listen_port"`
	TargetAddr   string              `json:"target_addr"`
	TargetPort   int                 `json:"target_port"`
	Enabled      bool                `json:"enabled"`
	SpeedLimit   int64               `json:"speed_limit"`
	Bandwidth    *bandwidthLimitView `json:"bandwidth_limit,omitempty"`
	TrafficQuota *trafficQuotaView   `json:"traffic_quota,omitempty"`
	MaxConn      int                 `json:"max_conn"`
	IdleTimeout  int                 `json:"idle_timeout,omitempty"`
	SourceIPMode engine.SourceIPMode `json:"source_ip_mode,omitempty"`
	SourceIPs    []string            `json:"source_ips,omitempty"`
	Domains      []string            `json:"domains,omitempty"`
	Remark       string              `json:"remark,omitempty"`
	Revision     int64               `json:"revision,omitempty"`
	CreatedTime  int64               `json:"created_time,omitempty"`
	UpdatedTime  int64               `json:"updated_time,omitempty"`
}

func newBandwidthLimitView(limit *engine.BandwidthLimit) *bandwidthLimitView {
	if limit == nil {
		return nil
	}
	view := &bandwidthLimitView{}
	if limit.Rate > 0 {
		view.Rate = limit.Rate.String()
	}
	if limit.Upload > 0 {
		view.Upload = limit.Upload.String()
	}
	if limit.Download > 0 {
		view.Download = limit.Download.String()
	}
	return view
}

func newTrafficQuotaView(quota *engine.TrafficQuota) *trafficQuotaView {
	if quota == nil {
		return nil
	}
	view := &trafficQuotaView{
		Timezone: quota.Timezone,
		Action:   quota.Action,
	}
	if quota.Daily > 0 {
		view.Daily = quota.Daily.String()
	}
	if quota.Monthly > 0 {
		view.Monthly = quota.Monthly.String()
	}
	return view
}

func newForwardingRuleView(rule engine.Rule) forwardingRuleView {
	view := forwardingRuleView{
		RuleID:       rule.RuleID,
		Name:         rule.Name,
		Protocol:     rule.Protocol,
		ListenAddr:   rule.ListenAddr,
		ListenPort:   rule.ListenPort,
		TargetAddr:   rule.TargetAddr,
		TargetPort:   rule.TargetPort,
		Enabled:      rule.Enabled,
		SpeedLimit:   rule.SpeedLimit,
		Bandwidth:    newBandwidthLimitView(rule.Bandwidth),
		TrafficQuota: newTrafficQuotaView(rule.TrafficQuota),
		MaxConn:      rule.MaxConn,
		IdleTimeout:  rule.IdleTimeout,
		SourceIPMode: rule.SourceIPMode,
		SourceIPs:    rule.SourceIPs,
		Domains:      rule.Domains,
		Remark:       rule.Remark,
		Revision:     rule.Revision,
		CreatedTime:  rule.CreatedTime,
		UpdatedTime:  rule.UpdatedTime,
	}
	if view.SourceIPs == nil {
		view.SourceIPs = []string{}
	}
	if view.Domains == nil {
		view.Domains = []string{}
	}
	return view
}

// ruleViewPtr converts an engine rule reference into the schema-accurate
// output view, preserving nil.
func ruleViewPtr(rule *engine.Rule) *forwardingRuleView {
	if rule == nil {
		return nil
	}
	view := newForwardingRuleView(*rule)
	return &view
}
