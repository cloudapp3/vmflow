package vmflow

import (
	"context"
	"crypto/tls"
	"sync"

	"github.com/cloudapp3/vmflow/engine"
)

// CertProvider is the certificate provider interface used by HTTPS rules.
// It is an alias of engine.CertProvider so embedders can depend on the
// top-level vmflow package without importing engine for certificate wiring.
type CertProvider = engine.CertProvider

// Runtime is a small embeddable facade over the forwarding engine.
//
// Use Runtime when vmflow is embedded into a larger control plane such as
// vmpulse. The larger application remains responsible for configuration,
// persistence, authentication, and business logic; Runtime owns only the
// in-process forwarding manager and real-time counters.
type Runtime struct {
	manager   *engine.Manager
	collector *engine.Collector
	mu        sync.RWMutex
	closed    bool
}

// Options controls construction of an embedded Runtime.
type Options struct {
	// Collector can be supplied when the host application wants to share or wrap
	// the vmflow in-memory traffic counters. If nil, a new collector is created.
	Collector *engine.Collector

	// CertProvider enables HTTPS rules. Leave nil when the embedded application
	// only uses TCP, UDP, tcp+udp, or HTTP proxy rules.
	CertProvider CertProvider
}

// NewRuntime creates a new embeddable forwarding runtime.
func NewRuntime(opts Options) *Runtime {
	collector := opts.Collector
	if collector == nil {
		collector = engine.NewCollector()
	}

	var manager *engine.Manager
	if opts.CertProvider != nil {
		manager = engine.NewManagerWithCert(collector, opts.CertProvider)
	} else {
		manager = engine.NewManager(collector)
	}

	return &Runtime{manager: manager, collector: collector}
}

// New creates a runtime with default options.
func New() *Runtime {
	return NewRuntime(Options{})
}

// Manager returns the underlying engine manager for advanced use cases.
// Prefer the facade methods on Runtime when possible.
func (r *Runtime) Manager() *engine.Manager {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.manager
}

// Collector returns the underlying in-memory collector.
func (r *Runtime) Collector() *engine.Collector {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.collector
}

// Apply applies a full replacement snapshot. This is the usual mode for an
// embedded control plane that calculates desired state from its own database.
func (r *Runtime) Apply(rules []engine.Rule) engine.ApplyResult {
	return r.ApplySnapshot(rules, engine.ApplySnapshotOptions{ReplaceAll: true})
}

// ApplySnapshot applies rules with explicit options.
func (r *Runtime) ApplySnapshot(rules []engine.Rule, opts engine.ApplySnapshotOptions) engine.ApplyResult {
	manager, ok := r.activeManager()
	if !ok {
		return closedApplyResult(rules)
	}
	return manager.ApplySnapshot(rules, opts)
}

// StartRule starts a single rule without replacing other rules.
func (r *Runtime) StartRule(rule engine.Rule) error {
	manager, ok := r.activeManager()
	if !ok {
		return ErrRuntimeClosed
	}
	return manager.StartRule(rule)
}

// RestartRule restarts or starts a single rule.
func (r *Runtime) RestartRule(rule engine.Rule) error {
	manager, ok := r.activeManager()
	if !ok {
		return ErrRuntimeClosed
	}
	return manager.RestartRule(rule)
}

// StopRule stops a single rule and keeps its counters.
func (r *Runtime) StopRule(ruleID string) {
	manager, ok := r.activeManager()
	if !ok {
		return
	}
	manager.StopRule(ruleID)
}

// RemoveRule stops a single rule and removes its counters.
func (r *Runtime) RemoveRule(ruleID string) {
	manager, ok := r.activeManager()
	if !ok {
		return
	}
	manager.RemoveRule(ruleID)
}

// StopAll stops all running rules and keeps the runtime reusable.
func (r *Runtime) StopAll() {
	manager, ok := r.activeManager()
	if !ok {
		return
	}
	manager.StopAll()
}

// Close stops all rules and marks the runtime as closed.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	manager := r.manager
	r.mu.Unlock()

	if manager != nil {
		manager.StopAll()
	}
	return nil
}

// Shutdown stops the runtime. The current implementation stops synchronously;
// the context is accepted to keep the embedding API forward-compatible with
// future graceful drain support.
func (r *Runtime) Shutdown(ctx context.Context) error {
	_ = ctx
	return r.Close()
}

// RunningRules returns the currently running rules sorted by rule_id.
func (r *Runtime) RunningRules() []engine.Rule {
	manager, ok := r.activeManager()
	if !ok {
		return nil
	}
	return manager.RunningRules()
}

// RunningCount returns the number of currently running rules.
func (r *Runtime) RunningCount() int {
	manager, ok := r.activeManager()
	if !ok {
		return 0
	}
	return manager.RunningCount()
}

// Snapshot returns one rule's in-memory traffic snapshot.
func (r *Runtime) Snapshot(ruleID string) engine.TrafficSnapshot {
	manager, ok := r.activeManager()
	if !ok {
		return engine.TrafficSnapshot{RuleID: ruleID}
	}
	return manager.Snapshot(ruleID)
}

// SnapshotAll returns all in-memory traffic snapshots sorted by rule_id.
func (r *Runtime) SnapshotAll() []engine.TrafficSnapshot {
	manager, ok := r.activeManager()
	if !ok {
		return nil
	}
	return manager.SnapshotAll()
}

func (r *Runtime) activeManager() (*engine.Manager, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.manager == nil {
		return nil, false
	}
	return r.manager, true
}

func closedApplyResult(rules []engine.Rule) engine.ApplyResult {
	result := engine.ApplyResult{
		FailedRules: len(rules),
		TotalRules:  len(rules),
	}
	if len(rules) == 0 {
		return result
	}
	result.Items = make([]engine.ApplyItemResult, 0, len(rules))
	for _, rule := range rules {
		rule = rule.Standardize()
		result.Items = append(result.Items, engine.ApplyItemResult{
			RuleID:   rule.RuleID,
			Revision: rule.Revision,
			Action:   engine.ApplyActionFailed,
			Status:   "failed",
			Error:    ErrRuntimeClosed.Error(),
		})
	}
	return result
}

// Ensure top-level CertProvider stays compatible with engine.CertProvider.
var _ CertProvider = certProviderFunc(nil)

type certProviderFunc func(*tls.ClientHelloInfo) (*tls.Certificate, error)

func (f certProviderFunc) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return f(hello)
}

func (f certProviderFunc) Obtain(ctx context.Context, domains []string) error {
	return nil
}
