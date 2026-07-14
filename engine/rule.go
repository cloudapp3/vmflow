package engine

import (
	"fmt"
	"strings"
)

type Protocol string

const (
	ProtocolTCP    Protocol = "tcp"
	ProtocolUDP    Protocol = "udp"
	ProtocolTCPUDP Protocol = "tcp+udp"
	ProtocolHTTP   Protocol = "http"
	ProtocolHTTPS  Protocol = "https"
)

type Rule struct {
	RuleID      string   `json:"rule_id" yaml:"rule_id"`
	Name        string   `json:"name" yaml:"name"`
	Protocol    Protocol `json:"protocol" yaml:"protocol"`
	ListenAddr  string   `json:"listen_addr" yaml:"listen_addr"`
	ListenPort  int      `json:"listen_port" yaml:"listen_port"`
	TargetAddr  string   `json:"target_addr" yaml:"target_addr"`
	TargetPort  int      `json:"target_port" yaml:"target_port"`
	Enabled     bool     `json:"enabled" yaml:"enabled"`
	SpeedLimit  int64    `json:"speed_limit" yaml:"speed_limit"`
	MaxConn     int      `json:"max_conn" yaml:"max_conn"`
	IdleTimeout int      `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
	Domains     []string `json:"domains,omitempty" yaml:"domains,omitempty"`
	Remark      string   `json:"remark,omitempty" yaml:"remark,omitempty"`
	Revision    int64    `json:"revision,omitempty" yaml:"revision,omitempty"`
	CreatedTime int64    `json:"created_time,omitempty" yaml:"created_time,omitempty"`
	UpdatedTime int64    `json:"updated_time,omitempty" yaml:"updated_time,omitempty"`
}

func (rule Rule) Standardize() Rule {
	rule.RuleID = strings.TrimSpace(rule.RuleID)
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Protocol = standardizeProtocol(rule.Protocol)
	rule.ListenAddr = strings.TrimSpace(rule.ListenAddr)
	rule.TargetAddr = strings.TrimSpace(rule.TargetAddr)
	rule.Remark = strings.TrimSpace(rule.Remark)
	return rule
}

func (rule Rule) Validate() error {
	rule = rule.Standardize()
	if rule.RuleID == "" {
		return fmt.Errorf("missing rule id")
	}
	if rule.Name == "" {
		return fmt.Errorf("missing rule name")
	}
	if rule.ListenPort <= 0 || rule.ListenPort > 65535 {
		return fmt.Errorf("listen_port out of range")
	}

	// http (forward proxy) and https (TLS termination / ACME) are disabled in
	// this build. Remove this check to re-enable them.
	if rule.Protocol == ProtocolHTTP || rule.Protocol == ProtocolHTTPS {
		return fmt.Errorf("protocol %s is disabled in this build", rule.Protocol)
	}

	isHTTPProxy := rule.Protocol == ProtocolHTTP

	if !isHTTPProxy {
		if rule.TargetAddr == "" {
			return fmt.Errorf("missing target addr")
		}
		if rule.TargetPort <= 0 || rule.TargetPort > 65535 {
			return fmt.Errorf("target_port out of range")
		}
	}

	if rule.Protocol == ProtocolHTTPS && len(rule.Domains) == 0 {
		return fmt.Errorf("https protocol requires at least one domain")
	}

	if rule.Protocol != ProtocolTCP && rule.Protocol != ProtocolUDP && rule.Protocol != ProtocolTCPUDP && rule.Protocol != ProtocolHTTP && rule.Protocol != ProtocolHTTPS {
		return fmt.Errorf("invalid protocol: %s", rule.Protocol)
	}
	if rule.SpeedLimit < 0 {
		return fmt.Errorf("speed_limit must be >= 0")
	}
	if rule.MaxConn < 0 {
		return fmt.Errorf("max_conn must be >= 0")
	}
	if rule.IdleTimeout < 0 {
		return fmt.Errorf("idle_timeout must be >= 0")
	}
	return nil
}

func (rule Rule) RuntimeEqual(other Rule) bool {
	left := rule.Standardize()
	right := other.Standardize()
	return left.Protocol == right.Protocol &&
		canonicalListenAddr(left.ListenAddr) == canonicalListenAddr(right.ListenAddr) &&
		left.ListenPort == right.ListenPort &&
		strings.EqualFold(left.TargetAddr, right.TargetAddr) &&
		left.TargetPort == right.TargetPort &&
		left.Enabled == right.Enabled &&
		left.SpeedLimit == right.SpeedLimit &&
		runtimeMaxConn(left) == runtimeMaxConn(right) &&
		left.IdleTimeout == right.IdleTimeout
}

func runtimeMaxConn(rule Rule) int {
	if rule.Protocol == ProtocolUDP && rule.MaxConn == 0 {
		return DefaultUDPRuleMaxSessions
	}
	return rule.MaxConn
}

type ApplySnapshotOptions struct {
	ReplaceAll bool `json:"replace_all"`
}

type ApplyAction string

const (
	ApplyActionStarted   ApplyAction = "started"
	ApplyActionRestarted ApplyAction = "restarted"
	ApplyActionStopped   ApplyAction = "stopped"
	ApplyActionRemoved   ApplyAction = "removed"
	ApplyActionUnchanged ApplyAction = "unchanged"
	ApplyActionFailed    ApplyAction = "failed"
)

type ApplyItemResult struct {
	RuleID   string      `json:"rule_id"`
	Revision int64       `json:"revision,omitempty"`
	Action   ApplyAction `json:"action"`
	Status   string      `json:"status"`
	Error    string      `json:"error,omitempty"`
}

type ApplyResult struct {
	AppliedRules int               `json:"applied_rules"`
	StoppedRules int               `json:"stopped_rules"`
	FailedRules  int               `json:"failed_rules"`
	TotalRules   int               `json:"total_rules"`
	Items        []ApplyItemResult `json:"items"`
}

// ApplyFailure identifies the operation that prevented a transactional
// snapshot from being committed. Validation failures are also listed in
// Apply.Items; this field points at the first failure that stopped the batch.
type ApplyFailure struct {
	RuleID   string `json:"rule_id,omitempty"`
	Revision int64  `json:"revision,omitempty"`
	Error    string `json:"error"`
}

// RollbackItemResult reports the outcome of reverting one successfully applied
// rule operation. Action is the original operation being reverted.
type RollbackItemResult struct {
	RuleID string      `json:"rule_id"`
	Action ApplyAction `json:"action"`
	Status string      `json:"status"`
	Error  string      `json:"error,omitempty"`
}

type RollbackResult struct {
	Attempted bool                 `json:"attempted"`
	Failed    bool                 `json:"failed"`
	Items     []RollbackItemResult `json:"items,omitempty"`
}

// TransactionalApplyResult separates the error that stopped the desired
// snapshot from any error encountered while restoring the previous snapshot.
type TransactionalApplyResult struct {
	Apply        ApplyResult    `json:"apply"`
	ApplyFailure *ApplyFailure  `json:"apply_failure,omitempty"`
	Rollback     RollbackResult `json:"rollback"`
}

type TrafficSnapshot struct {
	RuleID             string `json:"rule_id"`
	UploadBytes        int64  `json:"upload_bytes"`
	DownloadBytes      int64  `json:"download_bytes"`
	Conns              int64  `json:"conns"`
	UDPSessionRejected int64  `json:"udp_session_rejected_total,omitempty"`
	UDPPacketsDropped  int64  `json:"udp_packets_dropped_total,omitempty"`
	UpdatedTime        int64  `json:"updated_time"`
}

func standardizeProtocol(value Protocol) Protocol {
	switch strings.ToLower(strings.TrimSpace(string(value))) {
	case string(ProtocolTCP):
		return ProtocolTCP
	case string(ProtocolUDP):
		return ProtocolUDP
	case string(ProtocolTCPUDP):
		return ProtocolTCPUDP
	default:
		return Protocol(strings.ToLower(strings.TrimSpace(string(value))))
	}
}

func canonicalListenAddr(value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch normalized {
	case "", "0.0.0.0", "::":
		return ""
	default:
		return normalized
	}
}
