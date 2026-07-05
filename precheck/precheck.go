package precheck

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
)

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Item is one precheck finding.
type Item struct {
	Severity Severity `json:"severity"`
	Check    string   `json:"check"`
	RuleID   string   `json:"rule_id,omitempty"`
	Message  string   `json:"message"`
}

// Result is the complete rule/config precheck result.
type Result struct {
	OK            bool   `json:"ok"`
	ErrorCount    int    `json:"error_count"`
	WarningCount  int    `json:"warning_count"`
	CheckedRules  int    `json:"checked_rules"`
	CheckedTimeMS int64  `json:"checked_time_ms"`
	Items         []Item `json:"items"`
}

// Options controls precheck behavior.
type Options struct {
	// CheckBind tests whether desired listen endpoints can be bound. Endpoints
	// already owned by the same running rule are skipped to avoid false reload
	// failures.
	CheckBind bool

	// CheckTargetResolve tests target address resolution for forwarding rules.
	CheckTargetResolve bool
}

// DefaultOptions returns production-safe precheck defaults.
func DefaultOptions() Options {
	return Options{CheckBind: true, CheckTargetResolve: true}
}

// Error wraps a failed precheck result.
type Error struct {
	Result Result
}

func (e *Error) Error() string {
	return "precheck failed"
}

// CheckConfig validates a loaded config against currently running rules.
func CheckConfig(cfg config.File, running []engine.Rule, opts Options) Result {
	start := time.Now()
	checker := &checker{
		cfg:     cfg,
		running: running,
		opts:    opts,
		seen:    make(map[string]struct{}, len(cfg.Rules)),
	}
	checker.checkRules()
	checker.checkACME()

	result := Result{
		OK:            checker.errors == 0,
		ErrorCount:    checker.errors,
		WarningCount:  checker.warnings,
		CheckedRules:  len(cfg.Rules),
		CheckedTimeMS: time.Since(start).Milliseconds(),
		Items:         checker.items,
	}
	if result.Items == nil {
		result.Items = []Item{}
	}
	return result
}

type checker struct {
	cfg     config.File
	running []engine.Rule
	opts    Options

	seen      map[string]struct{}
	endpoints []endpoint
	items     []Item
	errors    int
	warnings  int
}

type endpoint struct {
	Network string
	Addr    string
	Port    int
	RuleID  string
}

func (c *checker) checkRules() {
	for _, rawRule := range c.cfg.Rules {
		rule := rawRule.Standardize()
		if _, ok := c.seen[rule.RuleID]; ok && rule.RuleID != "" {
			c.addError("duplicate_rule_id", rule.RuleID, "duplicate rule_id in desired snapshot")
		}
		if rule.RuleID != "" {
			c.seen[rule.RuleID] = struct{}{}
		}

		if err := rule.Validate(); err != nil {
			c.addError("rule_validate", rule.RuleID, err.Error())
			continue
		}

		if rule.ListenPort > 0 && rule.ListenPort < 1024 && rule.Enabled {
			c.addWarning("privileged_port", rule.RuleID, fmt.Sprintf("listen_port %d may require elevated privileges", rule.ListenPort))
		}

		if rule.Protocol == engine.ProtocolHTTPS {
			c.checkHTTPSDomains(rule)
		}

		for _, ep := range endpointsForRule(rule) {
			c.checkEndpointConflict(ep)
			c.endpoints = append(c.endpoints, ep)
			if c.opts.CheckBind && rule.Enabled && !c.ownedBySameRunningRule(ep) {
				c.checkBind(ep)
			}
		}

		if c.opts.CheckTargetResolve && rule.Enabled {
			c.checkTargetResolve(rule)
		}
	}
}

func (c *checker) checkEndpointConflict(ep endpoint) {
	for _, existing := range c.endpoints {
		if existing.Network != ep.Network || existing.Port != ep.Port {
			continue
		}
		if endpointAddrConflict(existing.Addr, ep.Addr) {
			c.addError("listen_conflict", ep.RuleID, fmt.Sprintf("listen %s/%s:%d conflicts with rule %s", ep.Network, displayAddr(ep.Addr), ep.Port, existing.RuleID))
		}
	}
}

func (c *checker) checkBind(ep endpoint) {
	addr := net.JoinHostPort(ep.Addr, strconv.Itoa(ep.Port))
	switch ep.Network {
	case "tcp":
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			c.addError("listen_bind", ep.RuleID, fmt.Sprintf("cannot bind tcp %s: %v", addr, err))
			return
		}
		_ = ln.Close()
	case "udp":
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			c.addError("listen_bind", ep.RuleID, fmt.Sprintf("cannot resolve udp listen %s: %v", addr, err))
			return
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			c.addError("listen_bind", ep.RuleID, fmt.Sprintf("cannot bind udp %s: %v", addr, err))
			return
		}
		_ = conn.Close()
	}
}

func (c *checker) checkTargetResolve(rule engine.Rule) {
	switch rule.Protocol {
	case engine.ProtocolHTTP:
		return
	case engine.ProtocolUDP:
		if _, err := net.ResolveUDPAddr("udp", net.JoinHostPort(rule.TargetAddr, strconv.Itoa(rule.TargetPort))); err != nil {
			c.addError("target_resolve", rule.RuleID, fmt.Sprintf("cannot resolve udp target: %v", err))
		}
	default:
		if _, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(rule.TargetAddr, strconv.Itoa(rule.TargetPort))); err != nil {
			c.addError("target_resolve", rule.RuleID, fmt.Sprintf("cannot resolve tcp target: %v", err))
		}
	}
}

func (c *checker) checkHTTPSDomains(rule engine.Rule) {
	seen := make(map[string]struct{}, len(rule.Domains))
	for _, raw := range rule.Domains {
		domain := strings.ToLower(strings.TrimSpace(raw))
		if domain == "" {
			c.addError("https_domain", rule.RuleID, "https domain cannot be empty")
			continue
		}
		if strings.ContainsAny(domain, " \t\r\n/") {
			c.addError("https_domain", rule.RuleID, fmt.Sprintf("invalid https domain: %q", raw))
			continue
		}
		if _, ok := seen[domain]; ok {
			c.addWarning("https_domain", rule.RuleID, fmt.Sprintf("duplicate https domain: %s", domain))
		}
		seen[domain] = struct{}{}
	}
}

func (c *checker) checkACME() {
	hasHTTPS := false
	for _, rule := range c.cfg.Rules {
		if rule.Standardize().Protocol == engine.ProtocolHTTPS && rule.Enabled {
			hasHTTPS = true
			break
		}
	}
	if !hasHTTPS || strings.TrimSpace(c.cfg.AcmeHTTP01Addr) == "" {
		return
	}
	addr := strings.TrimSpace(c.cfg.AcmeHTTP01Addr)
	host, portValue, err := net.SplitHostPort(addr)
	if err != nil {
		c.addError("acme_http01_addr", "", fmt.Sprintf("invalid acme_http01_addr %q: %v", addr, err))
		return
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port <= 0 || port > 65535 {
		c.addError("acme_http01_addr", "", fmt.Sprintf("invalid acme_http01_addr port %q", portValue))
		return
	}
	ep := endpoint{Network: "tcp", Addr: normalizeAddr(host), Port: port}
	for _, existing := range c.endpoints {
		if existing.Network == ep.Network && existing.Port == ep.Port && endpointAddrConflict(existing.Addr, ep.Addr) {
			c.addError("acme_http01_addr", "", fmt.Sprintf("acme_http01_addr %s conflicts with rule %s", addr, existing.RuleID))
		}
	}
	if c.opts.CheckBind {
		c.checkBind(ep)
	}
}

func (c *checker) ownedBySameRunningRule(ep endpoint) bool {
	for _, rule := range c.running {
		rule = rule.Standardize()
		if rule.RuleID != ep.RuleID {
			continue
		}
		for _, runningEP := range endpointsForRule(rule) {
			if runningEP.Network == ep.Network && runningEP.Port == ep.Port && normalizeAddr(runningEP.Addr) == normalizeAddr(ep.Addr) {
				return true
			}
		}
	}
	return false
}

func (c *checker) addError(check, ruleID, message string) {
	c.errors++
	c.items = append(c.items, Item{Severity: SeverityError, Check: check, RuleID: ruleID, Message: message})
}

func (c *checker) addWarning(check, ruleID, message string) {
	c.warnings++
	c.items = append(c.items, Item{Severity: SeverityWarning, Check: check, RuleID: ruleID, Message: message})
}

func endpointsForRule(rule engine.Rule) []endpoint {
	if !rule.Enabled {
		return nil
	}
	addr := normalizeAddr(rule.ListenAddr)
	switch rule.Protocol {
	case engine.ProtocolTCP, engine.ProtocolHTTP, engine.ProtocolHTTPS:
		return []endpoint{{Network: "tcp", Addr: addr, Port: rule.ListenPort, RuleID: rule.RuleID}}
	case engine.ProtocolUDP:
		return []endpoint{{Network: "udp", Addr: addr, Port: rule.ListenPort, RuleID: rule.RuleID}}
	case engine.ProtocolTCPUDP:
		return []endpoint{
			{Network: "tcp", Addr: addr, Port: rule.ListenPort, RuleID: rule.RuleID},
			{Network: "udp", Addr: addr, Port: rule.ListenPort, RuleID: rule.RuleID},
		}
	default:
		return nil
	}
}

func normalizeAddr(addr string) string {
	addr = strings.TrimSpace(strings.ToLower(addr))
	if addr == "0.0.0.0" || addr == "::" {
		return ""
	}
	return addr
}

func displayAddr(addr string) string {
	if addr == "" {
		return "*"
	}
	return addr
}

func endpointAddrConflict(a, b string) bool {
	a = normalizeAddr(a)
	b = normalizeAddr(b)
	return a == "" || b == "" || a == b
}

// SortItems sorts result items by severity/check/rule_id for stable output.
func SortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Severity != items[j].Severity {
			return items[i].Severity < items[j].Severity
		}
		if items[i].Check != items[j].Check {
			return items[i].Check < items[j].Check
		}
		return items[i].RuleID < items[j].RuleID
	})
}
