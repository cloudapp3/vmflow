package engine

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
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
	RuleID       string          `json:"rule_id" yaml:"rule_id"`
	Name         string          `json:"name" yaml:"name"`
	Protocol     Protocol        `json:"protocol" yaml:"protocol"`
	ListenAddr   string          `json:"listen_addr" yaml:"listen_addr"`
	ListenPort   int             `json:"listen_port" yaml:"listen_port"`
	TargetAddr   string          `json:"target_addr" yaml:"target_addr"`
	TargetPort   int             `json:"target_port" yaml:"target_port"`
	Enabled      bool            `json:"enabled" yaml:"enabled"`
	SpeedLimit   int64           `json:"speed_limit" yaml:"speed_limit"`
	Bandwidth    *BandwidthLimit `json:"bandwidth_limit,omitempty" yaml:"bandwidth_limit,omitempty"`
	TrafficQuota *TrafficQuota   `json:"traffic_quota,omitempty" yaml:"traffic_quota,omitempty"`
	MaxConn      int             `json:"max_conn" yaml:"max_conn"`
	IdleTimeout  int             `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
	SourceIPMode SourceIPMode    `json:"source_ip_mode,omitempty" yaml:"source_ip_mode,omitempty"`
	SourceIPs    []string        `json:"source_ips,omitempty" yaml:"source_ips,omitempty"`
	Domains      []string        `json:"domains,omitempty" yaml:"domains,omitempty"`
	Remark       string          `json:"remark,omitempty" yaml:"remark,omitempty"`
	Revision     int64           `json:"revision,omitempty" yaml:"revision,omitempty"`
	CreatedTime  int64           `json:"created_time,omitempty" yaml:"created_time,omitempty"`
	UpdatedTime  int64           `json:"updated_time,omitempty" yaml:"updated_time,omitempty"`
}

// BandwidthLimit caps aggregate traffic for an entire rule. Rate applies the
// same full-duplex limit to upload and download; Upload and Download provide
// asymmetric limits and cannot be combined with Rate.
type BandwidthLimit struct {
	Rate     ByteRate `json:"rate,omitempty" yaml:"rate,omitempty"`
	Upload   ByteRate `json:"upload,omitempty" yaml:"upload,omitempty"`
	Download ByteRate `json:"download,omitempty" yaml:"download,omitempty"`
}

func (limit BandwidthLimit) UploadBytesPerSecond() int64 {
	if limit.Upload > 0 {
		return int64(limit.Upload)
	}
	return int64(limit.Rate)
}

func (limit BandwidthLimit) DownloadBytesPerSecond() int64 {
	if limit.Download > 0 {
		return int64(limit.Download)
	}
	return int64(limit.Rate)
}

// TrafficQuota limits the combined successful upload and download payload for
// calendar-day and calendar-month periods in Timezone.
type TrafficQuota struct {
	Daily    ByteSize `json:"daily,omitempty" yaml:"daily,omitempty"`
	Monthly  ByteSize `json:"monthly,omitempty" yaml:"monthly,omitempty"`
	Timezone string   `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	Action   string   `json:"action,omitempty" yaml:"action,omitempty"`
}

func (rule Rule) Standardize() Rule {
	rule.RuleID = strings.TrimSpace(rule.RuleID)
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Protocol = standardizeProtocol(rule.Protocol)
	rule.ListenAddr = strings.TrimSpace(rule.ListenAddr)
	rule.TargetAddr = strings.TrimSpace(rule.TargetAddr)
	rule.SourceIPMode = normalizeSourceIPMode(rule.SourceIPMode)
	rule.SourceIPs = append([]string(nil), rule.SourceIPs...)
	for index := range rule.SourceIPs {
		rule.SourceIPs[index] = strings.TrimSpace(rule.SourceIPs[index])
	}
	rule.Domains = append([]string(nil), rule.Domains...)
	if rule.Bandwidth != nil {
		value := *rule.Bandwidth
		rule.Bandwidth = &value
	}
	if rule.TrafficQuota != nil {
		value := *rule.TrafficQuota
		value.Timezone = strings.TrimSpace(value.Timezone)
		if value.Timezone == "" {
			value.Timezone = "UTC"
		}
		value.Action = strings.ToLower(strings.TrimSpace(value.Action))
		if value.Action == "" {
			value.Action = "block"
		}
		rule.TrafficQuota = &value
	}
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
	if rule.Bandwidth != nil {
		limit := *rule.Bandwidth
		if limit.Rate < 0 || limit.Upload < 0 || limit.Download < 0 {
			return fmt.Errorf("bandwidth_limit values must be >= 0")
		}
		if limit.Rate > 0 && (limit.Upload > 0 || limit.Download > 0) {
			return fmt.Errorf("bandwidth_limit.rate cannot be combined with upload or download")
		}
		if limit.Rate == 0 && limit.Upload == 0 && limit.Download == 0 {
			return fmt.Errorf("bandwidth_limit must set rate, upload, or download")
		}
	}
	if rule.TrafficQuota != nil {
		quota := *rule.TrafficQuota
		if quota.Daily < 0 || quota.Monthly < 0 {
			return fmt.Errorf("traffic_quota values must be >= 0")
		}
		if quota.Daily == 0 && quota.Monthly == 0 {
			return fmt.Errorf("traffic_quota must set daily or monthly")
		}
		if quota.Action != "block" {
			return fmt.Errorf("traffic_quota.action must be block")
		}
		if _, err := time.LoadLocation(quota.Timezone); err != nil {
			return fmt.Errorf("traffic_quota.timezone %q is invalid: %w", quota.Timezone, err)
		}
	}
	if rule.MaxConn < 0 {
		return fmt.Errorf("max_conn must be >= 0")
	}
	if rule.IdleTimeout < 0 {
		return fmt.Errorf("idle_timeout must be >= 0")
	}
	if _, err := compileSourceIPMatcher(rule.SourceIPMode, rule.SourceIPs); err != nil {
		return err
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
		bandwidthLimitsEqual(left.Bandwidth, right.Bandwidth) &&
		trafficQuotasEqual(left.TrafficQuota, right.TrafficQuota) &&
		runtimeMaxConn(left) == runtimeMaxConn(right) &&
		left.IdleTimeout == right.IdleTimeout &&
		sourceIPPoliciesEqual(left, right)
}

func bandwidthLimitsEqual(left, right *BandwidthLimit) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func trafficQuotasEqual(left, right *TrafficQuota) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
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
	RuleID             string                `json:"rule_id"`
	UploadBytes        int64                 `json:"upload_bytes"`
	DownloadBytes      int64                 `json:"download_bytes"`
	Conns              int64                 `json:"conns"`
	SourceIPDenied     int64                 `json:"source_ip_denied_total,omitempty"`
	UDPSessionRejected int64                 `json:"udp_session_rejected_total,omitempty"`
	UDPPacketsDropped  int64                 `json:"udp_packets_dropped_total,omitempty"`
	QuotaRejectedBytes int64                 `json:"quota_rejected_bytes_total,omitempty"`
	Quota              *TrafficQuotaSnapshot `json:"quota,omitempty"`
	UpdatedTime        int64                 `json:"updated_time"`
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
