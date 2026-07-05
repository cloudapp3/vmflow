package engine

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Runner interface {
	Start() error
	Stop()
}

type managedRule struct {
	rule    Rule
	runners []Runner
}

type Manager struct {
	mu        sync.RWMutex
	applyMu   sync.Mutex
	running   map[string]*managedRule
	collector *Collector
	certMgr   CertProvider
}

type CertProvider interface {
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
	Obtain(ctx context.Context, domains []string) error
}

func NewManagerWithCert(collector *Collector, certMgr CertProvider) *Manager {
	if collector == nil {
		collector = NewCollector()
	}
	return &Manager{
		running:   make(map[string]*managedRule),
		collector: collector,
		certMgr:   certMgr,
	}
}

func NewManager(collector *Collector) *Manager {
	if collector == nil {
		collector = NewCollector()
	}
	return &Manager{
		running:   make(map[string]*managedRule),
		collector: collector,
	}
}

func (manager *Manager) StartRule(rule Rule) error {
	manager.applyMu.Lock()
	defer manager.applyMu.Unlock()
	return manager.startRuleInternal(rule)
}

func (manager *Manager) startRuleInternal(rule Rule) error {
	rule = rule.Standardize()
	if err := rule.Validate(); err != nil {
		return err
	}
	manager.collector.EnsureRule(rule.RuleID)
	if !rule.Enabled {
		return nil
	}

	manager.mu.RLock()
	_, exists := manager.running[rule.RuleID]
	manager.mu.RUnlock()
	if exists {
		return fmt.Errorf("rule already running: %s", rule.RuleID)
	}

	runners, err := manager.buildRunners(rule)
	if err != nil {
		return err
	}
	if err := startRunnerSet(runners); err != nil {
		return err
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, exists := manager.running[rule.RuleID]; exists {
		stopRunnerSet(runners)
		return fmt.Errorf("rule already running: %s", rule.RuleID)
	}
	manager.running[rule.RuleID] = &managedRule{rule: rule, runners: runners}
	return nil
}

func (manager *Manager) RestartRule(rule Rule) error {
	manager.applyMu.Lock()
	defer manager.applyMu.Unlock()
	return manager.restartRuleInternal(rule)
}

func (manager *Manager) restartRuleInternal(rule Rule) error {
	rule = rule.Standardize()
	if err := rule.Validate(); err != nil {
		return err
	}
	manager.collector.EnsureRule(rule.RuleID)
	current := manager.detachRule(rule.RuleID)
	if current != nil {
		stopRunnerSet(current.runners)
		manager.collector.SetConns(rule.RuleID, 0)
	}
	if !rule.Enabled {
		return nil
	}

	runners, err := manager.buildRunners(rule)
	if err != nil {
		return err
	}
	if err := startRunnerSet(runners); err != nil {
		return err
	}

	manager.mu.Lock()
	if _, exists := manager.running[rule.RuleID]; exists {
		manager.mu.Unlock()
		stopRunnerSet(runners)
		return fmt.Errorf("rule already running: %s", rule.RuleID)
	}
	manager.running[rule.RuleID] = &managedRule{rule: rule, runners: runners}
	manager.mu.Unlock()
	return nil
}

func (manager *Manager) StopRule(ruleID string) {
	manager.applyMu.Lock()
	defer manager.applyMu.Unlock()
	manager.stopRuleInternal(ruleID)
}

func (manager *Manager) stopRuleInternal(ruleID string) {
	ruleID = strings.TrimSpace(ruleID)
	if ruleID == "" {
		return
	}
	current := manager.detachRule(ruleID)
	if current != nil {
		stopRunnerSet(current.runners)
	}
	manager.collector.SetConns(ruleID, 0)
}

func (manager *Manager) RemoveRule(ruleID string) {
	manager.applyMu.Lock()
	defer manager.applyMu.Unlock()
	manager.stopRuleInternal(ruleID)
	manager.collector.RemoveRule(ruleID)
}

func (manager *Manager) StopAll() {
	manager.applyMu.Lock()
	defer manager.applyMu.Unlock()
	manager.mu.Lock()
	entries := make([]*managedRule, 0, len(manager.running))
	for _, item := range manager.running {
		entries = append(entries, item)
	}
	manager.running = make(map[string]*managedRule)
	manager.mu.Unlock()

	for _, item := range entries {
		stopRunnerSet(item.runners)
		manager.collector.SetConns(item.rule.RuleID, 0)
	}
}

func (manager *Manager) RunningRules() []Rule {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	rules := make([]Rule, 0, len(manager.running))
	for _, item := range manager.running {
		rules = append(rules, item.rule)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	return rules
}

func (manager *Manager) RunningCount() int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return len(manager.running)
}

func (manager *Manager) Snapshot(ruleID string) TrafficSnapshot {
	return manager.collector.Snapshot(ruleID)
}

func (manager *Manager) SnapshotAll() []TrafficSnapshot {
	return manager.collector.SnapshotAll()
}

func (manager *Manager) ApplySnapshot(rules []Rule, opts ApplySnapshotOptions) ApplyResult {
	manager.applyMu.Lock()
	defer manager.applyMu.Unlock()

	result := ApplyResult{TotalRules: len(rules)}
	seen := make(map[string]struct{}, len(rules))

	for _, rawRule := range rules {
		rule := rawRule.Standardize()
		item := ApplyItemResult{RuleID: rule.RuleID, Revision: rule.Revision}

		if rule.RuleID == "" {
			item.Action = ApplyActionFailed
			item.Status = "failed"
			item.Error = "missing rule id"
			result.FailedRules++
			result.Items = append(result.Items, item)
			continue
		}
		if _, duplicated := seen[rule.RuleID]; duplicated {
			item.Action = ApplyActionFailed
			item.Status = "failed"
			item.Error = "duplicate rule id in snapshot"
			result.FailedRules++
			result.Items = append(result.Items, item)
			continue
		}
		seen[rule.RuleID] = struct{}{}

		if err := rule.Validate(); err != nil {
			item.Action = ApplyActionFailed
			item.Status = "failed"
			item.Error = err.Error()
			result.FailedRules++
			result.Items = append(result.Items, item)
			continue
		}

		current, running := manager.runningRule(rule.RuleID)
		if !rule.Enabled {
			manager.collector.EnsureRule(rule.RuleID)
			if running {
				manager.stopRuleInternal(rule.RuleID)
				item.Action = ApplyActionStopped
				item.Status = "ok"
				result.StoppedRules++
			} else {
				item.Action = ApplyActionUnchanged
				item.Status = "ok"
			}
			result.Items = append(result.Items, item)
			continue
		}

		if running && current.rule.RuntimeEqual(rule) {
			manager.updateRule(rule)
			manager.collector.EnsureRule(rule.RuleID)
			item.Action = ApplyActionUnchanged
			item.Status = "ok"
			result.Items = append(result.Items, item)
			continue
		}

		var err error
		if running {
			err = manager.restartRuleInternal(rule)
			item.Action = ApplyActionRestarted
		} else {
			err = manager.startRuleInternal(rule)
			item.Action = ApplyActionStarted
		}
		if err != nil {
			item.Action = ApplyActionFailed
			item.Status = "failed"
			item.Error = err.Error()
			result.FailedRules++
		} else {
			item.Status = "ok"
			result.AppliedRules++
		}
		result.Items = append(result.Items, item)
	}

	if opts.ReplaceAll {
		for _, ruleID := range manager.runningRuleIDs() {
			if _, ok := seen[ruleID]; ok {
				continue
			}
			manager.stopRuleInternal(ruleID)
			manager.collector.RemoveRule(ruleID)
			result.StoppedRules++
			result.Items = append(result.Items, ApplyItemResult{
				RuleID: ruleID,
				Action: ApplyActionRemoved,
				Status: "ok",
			})
		}
	}

	return result
}

func (manager *Manager) buildRunners(rule Rule) ([]Runner, error) {
	runners := make([]Runner, 0, 2)
	switch rule.Protocol {
	case ProtocolTCP:
		runners = append(runners, newTCPRunner(rule, manager.collector))
	case ProtocolUDP:
		runners = append(runners, newUDPRunner(rule, manager.collector))
	case ProtocolTCPUDP:
		runners = append(runners, newTCPRunner(rule, manager.collector), newUDPRunner(rule, manager.collector))
	case ProtocolHTTP:
		runners = append(runners, newHTTPProxyRunner(rule, manager.collector))
	case ProtocolHTTPS:
		runners = append(runners, newHTTPSRunner(rule, manager.collector, manager.certMgr))
	default:
		return nil, fmt.Errorf("invalid protocol: %s", rule.Protocol)
	}
	return runners, nil
}

func (manager *Manager) detachRule(ruleID string) *managedRule {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current := manager.running[ruleID]
	delete(manager.running, ruleID)
	return current
}

func (manager *Manager) runningRule(ruleID string) (*managedRule, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	current, ok := manager.running[ruleID]
	return current, ok
}

func (manager *Manager) updateRule(rule Rule) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current, ok := manager.running[rule.RuleID]
	if !ok {
		return
	}
	current.rule = rule
}

func (manager *Manager) runningRuleIDs() []string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	ids := make([]string, 0, len(manager.running))
	for ruleID := range manager.running {
		ids = append(ids, ruleID)
	}
	sort.Strings(ids)
	return ids
}

func startRunnerSet(runners []Runner) error {
	started := make([]Runner, 0, len(runners))
	for _, item := range runners {
		if err := item.Start(); err != nil {
			stopRunnerSet(started)
			return err
		}
		started = append(started, item)
	}
	return nil
}

func stopRunnerSet(runners []Runner) {
	for _, item := range runners {
		item.Stop()
	}
}
