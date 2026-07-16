package engine

import (
	"context"
	"crypto/tls"
	"errors"
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
	mu            sync.RWMutex
	applyMu       sync.Mutex
	running       map[string]*managedRule
	collector     *Collector
	certMgr       CertProvider
	udpBudget     *udpSessionBudget
	runnerBuilder func(Rule) ([]Runner, error)
}

type CertProvider interface {
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
	Obtain(ctx context.Context, domains []string) error
}

func NewManagerWithCert(collector *Collector, certMgr CertProvider) *Manager {
	return NewManagerWithCertOptions(collector, certMgr, ManagerOptions{})
}

// NewManagerWithCertOptions creates a manager with a certificate provider and
// explicit manager-wide resource limits.
func NewManagerWithCertOptions(collector *Collector, certMgr CertProvider, opts ManagerOptions) *Manager {
	if collector == nil {
		collector = NewCollector()
	}
	return &Manager{
		running:   make(map[string]*managedRule),
		collector: collector,
		certMgr:   certMgr,
		udpBudget: newUDPSessionBudget(opts.UDPMaxSessions),
	}
}

func NewManager(collector *Collector) *Manager {
	return NewManagerWithOptions(collector, ManagerOptions{})
}

// NewManagerWithOptions creates a manager with explicit manager-wide resource
// limits. Zero-valued limits use safe defaults.
func NewManagerWithOptions(collector *Collector, opts ManagerOptions) *Manager {
	if collector == nil {
		collector = NewCollector()
	}
	return &Manager{
		running:   make(map[string]*managedRule),
		collector: collector,
		udpBudget: newUDPSessionBudget(opts.UDPMaxSessions),
	}
}

// SetUDPMaxSessions updates admission for future UDP sessions. Existing
// sessions are not terminated when the limit is lowered; new sessions remain
// blocked until usage falls below the new limit.
func (manager *Manager) SetUDPMaxSessions(limit int) {
	if manager == nil {
		return
	}
	manager.udpBudget.setLimit(limit)
}

// UDPMaxSessions reports the configured global limit and current usage.
func (manager *Manager) UDPMaxSessions() (limit, active int) {
	if manager == nil {
		return DefaultUDPGlobalMaxSessions, 0
	}
	return manager.udpBudget.snapshot()
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
	if !rule.Enabled {
		if _, running := manager.runningRule(rule.RuleID); !running {
			manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
		}
		return nil
	}

	manager.mu.RLock()
	_, exists := manager.running[rule.RuleID]
	manager.mu.RUnlock()
	if exists {
		return fmt.Errorf("rule already running: %s", rule.RuleID)
	}
	manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)

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
	if !rule.Enabled {
		manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
		manager.stopRuleInternal(rule.RuleID)
		return nil
	}

	runners, err := manager.buildRunners(rule)
	if err != nil {
		return err
	}
	current := manager.detachRule(rule.RuleID)
	if current != nil {
		stopRunnerSet(current.runners)
		manager.collector.SetConns(rule.RuleID, 0)
	}
	manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
	if err := startRunnerSet(runners); err != nil {
		return manager.restoreRuleAfterRestartFailure(current, err)
	}

	manager.mu.Lock()
	if _, exists := manager.running[rule.RuleID]; exists {
		manager.mu.Unlock()
		stopRunnerSet(runners)
		return manager.restoreRuleAfterRestartFailure(current, fmt.Errorf("rule already running: %s", rule.RuleID))
	}
	manager.running[rule.RuleID] = &managedRule{rule: rule, runners: runners}
	manager.mu.Unlock()
	return nil
}

type restartRuleError struct {
	applyErr    error
	rollbackErr error
}

func (err *restartRuleError) Error() string {
	if err.rollbackErr == nil {
		return fmt.Sprintf("restart failed: %v; previous rule restored", err.applyErr)
	}
	return fmt.Sprintf("restart failed: %v; restore previous rule failed: %v", err.applyErr, err.rollbackErr)
}

func (err *restartRuleError) Unwrap() error { return err.applyErr }

func (manager *Manager) restoreRuleAfterRestartFailure(previous *managedRule, applyErr error) error {
	if previous == nil {
		return applyErr
	}
	manager.collector.EnsureRuleProtocol(previous.rule.RuleID, previous.rule.Protocol)
	runners, err := manager.buildRunners(previous.rule)
	if err == nil {
		err = startRunnerSet(runners)
	}
	if err != nil {
		return &restartRuleError{applyErr: applyErr, rollbackErr: err}
	}
	manager.mu.Lock()
	manager.running[previous.rule.RuleID] = &managedRule{rule: previous.rule, runners: runners}
	manager.mu.Unlock()
	return &restartRuleError{applyErr: applyErr}
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
		rule := item.rule
		rule.Domains = append([]string(nil), item.rule.Domains...)
		rule.SourceIPs = append([]string(nil), item.rule.SourceIPs...)
		rules = append(rules, rule)
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

// RuleProtocols returns protocol metadata for running, stopped, and disabled
// rules whose counters are still retained by this manager.
func (manager *Manager) RuleProtocols() map[string]Protocol {
	if manager == nil || manager.collector == nil {
		return nil
	}
	return manager.collector.RuleProtocols()
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
			manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
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
			manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
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

type rollbackStep struct {
	ruleID string
	action ApplyAction
	undo   func() error
}

// SnapshotApplyTransaction keeps a successful runtime apply reversible until
// its caller commits the associated external state (for example a config file
// rename). It owns Manager.applyMu until Commit or Rollback is called.
type SnapshotApplyTransaction struct {
	mu sync.Mutex

	manager        *Manager
	journal        []rollbackStep
	countersBefore collectorStateSnapshot
	closed         bool
}

// Commit makes the applied runtime snapshot final and releases the manager.
func (transaction *SnapshotApplyTransaction) Commit() {
	if transaction == nil {
		return
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return
	}
	transaction.closed = true
	transaction.manager.applyMu.Unlock()
}

// Rollback restores the runtime state captured before BeginApplySnapshotTransactional.
// It is idempotent; calling it after Commit returns an unattempted result.
func (transaction *SnapshotApplyTransaction) Rollback() RollbackResult {
	if transaction == nil {
		return RollbackResult{}
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return RollbackResult{}
	}
	transaction.closed = true
	result := RollbackResult{Attempted: true}
	transaction.manager.rollbackSteps(&result, transaction.journal, transaction.countersBefore)
	transaction.manager.applyMu.Unlock()
	return result
}

// ApplySnapshotTransactional applies a complete desired snapshot as one
// operation. Every rule is validated before runtime state changes. If a later
// operation fails, prior operations are undone in reverse order and collector
// state is restored to its pre-apply snapshot on a best-effort basis.
func (manager *Manager) ApplySnapshotTransactional(rules []Rule, opts ApplySnapshotOptions) TransactionalApplyResult {
	pending, result := manager.BeginApplySnapshotTransactional(rules, opts)
	if pending != nil {
		pending.Commit()
	}
	return result
}

// BeginApplySnapshotTransactional applies a desired snapshot while retaining
// an undo journal. A non-nil transaction means the apply succeeded and the
// caller must call Commit or Rollback. Failed applies are rolled back before
// this method returns and produce a nil transaction.
func (manager *Manager) BeginApplySnapshotTransactional(rules []Rule, opts ApplySnapshotOptions) (*SnapshotApplyTransaction, TransactionalApplyResult) {
	manager.applyMu.Lock()
	result, journal, countersBefore := manager.applySnapshotTransactionalLocked(rules, opts)
	if result.ApplyFailure != nil {
		manager.applyMu.Unlock()
		return nil, result
	}
	return &SnapshotApplyTransaction{
		manager:        manager,
		journal:        journal,
		countersBefore: countersBefore,
	}, result
}

func (manager *Manager) applySnapshotTransactionalLocked(rules []Rule, opts ApplySnapshotOptions) (TransactionalApplyResult, []rollbackStep, collectorStateSnapshot) {
	desired, result, validationFailure := validateTransactionalSnapshot(rules)
	transaction := TransactionalApplyResult{Apply: result}
	if validationFailure != nil {
		transaction.ApplyFailure = validationFailure
		return transaction, nil, collectorStateSnapshot{}
	}

	countersBefore := snapshotCollectorState(manager.collector)
	journal := make([]rollbackStep, 0, len(desired))
	seen := make(map[string]struct{}, len(desired))

	for _, rule := range desired {
		seen[rule.RuleID] = struct{}{}
		item := ApplyItemResult{RuleID: rule.RuleID, Revision: rule.Revision}
		current, running := manager.runningRule(rule.RuleID)

		if !rule.Enabled {
			manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
			if running {
				previous := current.rule
				manager.stopRuleInternal(rule.RuleID)
				journal = append(journal, rollbackStep{
					ruleID: rule.RuleID,
					action: ApplyActionStopped,
					undo: func() error {
						return manager.startRuleInternal(previous)
					},
				})
				item.Action = ApplyActionStopped
				transaction.Apply.StoppedRules++
			} else {
				item.Action = ApplyActionUnchanged
			}
			item.Status = "ok"
			transaction.Apply.Items = append(transaction.Apply.Items, item)
			continue
		}

		if running && current.rule.RuntimeEqual(rule) {
			previous := current.rule
			manager.updateRule(rule)
			manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
			journal = append(journal, rollbackStep{
				ruleID: rule.RuleID,
				action: ApplyActionUnchanged,
				undo: func() error {
					manager.updateRule(previous)
					return nil
				},
			})
			item.Action = ApplyActionUnchanged
			item.Status = "ok"
			transaction.Apply.Items = append(transaction.Apply.Items, item)
			continue
		}

		var err error
		if running {
			previous := current.rule
			err = manager.restartRuleInternal(rule)
			if err == nil {
				journal = append(journal, rollbackStep{
					ruleID: rule.RuleID,
					action: ApplyActionRestarted,
					undo: func() error {
						return manager.restartRuleInternal(previous)
					},
				})
				item.Action = ApplyActionRestarted
			}
		} else {
			err = manager.startRuleInternal(rule)
			if err == nil {
				journal = append(journal, rollbackStep{
					ruleID: rule.RuleID,
					action: ApplyActionStarted,
					undo: func() error {
						manager.stopRuleInternal(rule.RuleID)
						return nil
					},
				})
				item.Action = ApplyActionStarted
			}
		}
		if err != nil {
			applyErr := transactionalApplyError(err)
			item.Action = ApplyActionFailed
			item.Status = "failed"
			item.Error = applyErr.Error()
			transaction.Apply.FailedRules++
			transaction.Apply.Items = append(transaction.Apply.Items, item)
			transaction.ApplyFailure = &ApplyFailure{RuleID: rule.RuleID, Revision: rule.Revision, Error: applyErr.Error()}
			manager.rollbackTransaction(&transaction, journal, countersBefore, rule.RuleID, err)
			return transaction, nil, collectorStateSnapshot{}
		}

		item.Status = "ok"
		transaction.Apply.AppliedRules++
		transaction.Apply.Items = append(transaction.Apply.Items, item)
	}

	if opts.ReplaceAll {
		removed := make(map[string]struct{})
		for _, ruleID := range manager.runningRuleIDs() {
			if _, ok := seen[ruleID]; ok {
				continue
			}
			current, _ := manager.runningRule(ruleID)
			previous := current.rule
			manager.stopRuleInternal(ruleID)
			manager.collector.RemoveRule(ruleID)
			journal = append(journal, rollbackStep{
				ruleID: ruleID,
				action: ApplyActionRemoved,
				undo: func() error {
					return manager.startRuleInternal(previous)
				},
			})
			transaction.Apply.StoppedRules++
			transaction.Apply.Items = append(transaction.Apply.Items, ApplyItemResult{
				RuleID: ruleID,
				Action: ApplyActionRemoved,
				Status: "ok",
			})
			removed[ruleID] = struct{}{}
		}
		for _, ruleID := range manager.knownRuleIDs() {
			if _, ok := seen[ruleID]; ok {
				continue
			}
			if _, ok := removed[ruleID]; ok {
				continue
			}
			manager.collector.RemoveRule(ruleID)
			transaction.Apply.StoppedRules++
			transaction.Apply.Items = append(transaction.Apply.Items, ApplyItemResult{
				RuleID: ruleID,
				Action: ApplyActionRemoved,
				Status: "ok",
			})
		}
	}

	return transaction, journal, countersBefore
}

func validateTransactionalSnapshot(rules []Rule) ([]Rule, ApplyResult, *ApplyFailure) {
	desired := make([]Rule, 0, len(rules))
	result := ApplyResult{TotalRules: len(rules)}
	seen := make(map[string]struct{}, len(rules))
	var firstFailure *ApplyFailure

	for _, rawRule := range rules {
		rule := rawRule.Standardize()
		desired = append(desired, rule)
		item := ApplyItemResult{RuleID: rule.RuleID, Revision: rule.Revision}
		var err error
		switch {
		case rule.RuleID == "":
			err = errors.New("missing rule id")
		case hasRuleID(seen, rule.RuleID):
			err = errors.New("duplicate rule id in snapshot")
		default:
			seen[rule.RuleID] = struct{}{}
			err = rule.Validate()
		}
		if err == nil {
			continue
		}
		item.Action = ApplyActionFailed
		item.Status = "failed"
		item.Error = err.Error()
		result.FailedRules++
		result.Items = append(result.Items, item)
		if firstFailure == nil {
			firstFailure = &ApplyFailure{RuleID: rule.RuleID, Revision: rule.Revision, Error: err.Error()}
		}
	}
	return desired, result, firstFailure
}

func hasRuleID(seen map[string]struct{}, ruleID string) bool {
	_, ok := seen[ruleID]
	return ok
}

func transactionalApplyError(err error) error {
	var restartErr *restartRuleError
	if errors.As(err, &restartErr) {
		return restartErr.applyErr
	}
	return err
}

func (manager *Manager) rollbackTransaction(transaction *TransactionalApplyResult, journal []rollbackStep, countersBefore collectorStateSnapshot, failedRuleID string, applyErr error) {
	transaction.Rollback.Attempted = true
	var restartErr *restartRuleError
	if errors.As(applyErr, &restartErr) {
		item := RollbackItemResult{RuleID: failedRuleID, Action: ApplyActionRestarted, Status: "ok"}
		if restartErr.rollbackErr != nil {
			item.Status = "failed"
			item.Error = restartErr.rollbackErr.Error()
			transaction.Rollback.Failed = true
		}
		transaction.Rollback.Items = append(transaction.Rollback.Items, item)
	}

	manager.rollbackSteps(&transaction.Rollback, journal, countersBefore)
}

func (manager *Manager) rollbackSteps(result *RollbackResult, journal []rollbackStep, countersBefore collectorStateSnapshot) {
	for i := len(journal) - 1; i >= 0; i-- {
		step := journal[i]
		item := RollbackItemResult{RuleID: step.ruleID, Action: step.action, Status: "ok"}
		if err := step.undo(); err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			result.Failed = true
		}
		result.Items = append(result.Items, item)
	}
	restoreCollectorState(manager.collector, countersBefore)
	// A failed rollback can leave the last working replacement active. Keep
	// protocol metadata aligned with the runner that is actually serving.
	for _, rule := range manager.RunningRules() {
		manager.collector.EnsureRuleProtocol(rule.RuleID, rule.Protocol)
	}
}

type ruleCounterState struct {
	counter            *ruleCounter
	uploadTotal        int64
	downloadTotal      int64
	sourceIPDenied     int64
	udpSessionRejected int64
	udpPacketsDropped  int64
}

type collectorStateSnapshot struct {
	counters  map[string]ruleCounterState
	protocols map[string]Protocol
}

func snapshotCollectorState(collector *Collector) collectorStateSnapshot {
	snapshot := collectorStateSnapshot{
		counters:  make(map[string]ruleCounterState),
		protocols: make(map[string]Protocol),
	}
	if collector == nil {
		return snapshot
	}
	collector.mu.RLock()
	defer collector.mu.RUnlock()
	for ruleID, counter := range collector.counters {
		snapshot.counters[ruleID] = ruleCounterState{
			counter:            counter,
			uploadTotal:        counter.uploadTotal.Load(),
			downloadTotal:      counter.downloadTotal.Load(),
			sourceIPDenied:     counter.sourceIPDenied.Load(),
			udpSessionRejected: counter.udpSessionRejected.Load(),
			udpPacketsDropped:  counter.udpPacketsDropped.Load(),
		}
	}
	for ruleID, protocol := range collector.protocols {
		snapshot.protocols[ruleID] = protocol
	}
	return snapshot
}

func restoreCollectorState(collector *Collector, snapshot collectorStateSnapshot) {
	if collector == nil {
		return
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	for ruleID := range collector.counters {
		if _, ok := snapshot.counters[ruleID]; !ok {
			delete(collector.counters, ruleID)
		}
	}
	for ruleID, state := range snapshot.counters {
		counter := collector.counters[ruleID]
		if counter == state.counter {
			// This counter survived the transaction and may have received live
			// traffic while other rules were being applied. Keep those monotonic
			// values rather than rewinding the entire collector to the snapshot.
			continue
		}
		if counter == nil {
			counter = &ruleCounter{}
			collector.counters[ruleID] = counter
		}
		// RemoveRule discards the original counter. A rollback that recreates
		// the rule gets a new counter, so seed that replacement from the last
		// pre-transaction values.
		// Merge the deleted counter's cumulative history into any traffic the
		// restored runner has already recorded. Its live connection count and
		// timestamp must not be overwritten after the listener is active.
		counter.uploadTotal.Add(state.uploadTotal)
		counter.downloadTotal.Add(state.downloadTotal)
		counter.sourceIPDenied.Add(state.sourceIPDenied)
		counter.udpSessionRejected.Add(state.udpSessionRejected)
		counter.udpPacketsDropped.Add(state.udpPacketsDropped)
	}
	collector.protocols = make(map[string]Protocol, len(snapshot.protocols))
	for ruleID, protocol := range snapshot.protocols {
		collector.protocols[ruleID] = protocol
	}
}

func (manager *Manager) buildRunners(rule Rule) ([]Runner, error) {
	if manager.runnerBuilder != nil {
		return manager.runnerBuilder(rule)
	}
	runners := make([]Runner, 0, 2)
	switch rule.Protocol {
	case ProtocolTCP:
		runners = append(runners, newTCPRunner(rule, manager.collector))
	case ProtocolUDP:
		runners = append(runners, newUDPRunnerWithBudget(rule, manager.collector, manager.udpBudget))
	case ProtocolTCPUDP:
		runners = append(runners, newTCPRunner(rule, manager.collector), newUDPRunnerWithBudget(rule, manager.collector, manager.udpBudget))
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

func (manager *Manager) knownRuleIDs() []string {
	protocols := manager.collector.RuleProtocols()
	ids := make([]string, 0, len(protocols))
	for ruleID := range protocols {
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
