package engine

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type scriptedRunnerFactory struct {
	events      []string
	startErrors map[string][]error
}

func newScriptedManager(collector *Collector, script *scriptedRunnerFactory) *Manager {
	manager := NewManager(collector)
	manager.runnerBuilder = func(rule Rule) ([]Runner, error) {
		return []Runner{&scriptedRunner{name: rule.Name, factory: script}}, nil
	}
	return manager
}

type scriptedRunner struct {
	name    string
	factory *scriptedRunnerFactory
}

func (runner *scriptedRunner) Start() error {
	runner.factory.events = append(runner.factory.events, "start:"+runner.name)
	errorsForRule := runner.factory.startErrors[runner.name]
	if len(errorsForRule) == 0 {
		return nil
	}
	err := errorsForRule[0]
	runner.factory.startErrors[runner.name] = errorsForRule[1:]
	return err
}

func (runner *scriptedRunner) Stop() {
	runner.factory.events = append(runner.factory.events, "stop:"+runner.name)
}

func transactionRule(ruleID, name string, targetPort int) Rule {
	return Rule{
		RuleID:     ruleID,
		Name:       name,
		Protocol:   ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: 12000 + len(ruleID),
		TargetAddr: "127.0.0.1",
		TargetPort: targetPort,
		Enabled:    true,
	}
}

func TestStopAllPreservesCumulativeCounters(t *testing.T) {
	collector := NewCollector()
	manager := newScriptedManager(collector, &scriptedRunnerFactory{startErrors: make(map[string][]error)})
	rule := transactionRule("persisted", "persisted", 21000)
	if err := manager.StartRule(rule); err != nil {
		t.Fatal(err)
	}
	collector.AddUpload(rule.RuleID, 100)
	collector.AddDownload(rule.RuleID, 200)
	collector.SetConns(rule.RuleID, 3)

	manager.StopAll()
	snapshot := manager.Snapshot(rule.RuleID)
	if snapshot.UploadBytes != 100 || snapshot.DownloadBytes != 200 {
		t.Fatalf("StopAll discarded cumulative counters: %+v", snapshot)
	}
	if snapshot.Conns != 0 {
		t.Fatalf("StopAll left live connections: %+v", snapshot)
	}
}

func TestManagerSourceIPPolicySnapshotIsolation(t *testing.T) {
	manager := newScriptedManager(NewCollector(), &scriptedRunnerFactory{startErrors: make(map[string][]error)})
	defer manager.StopAll()
	rule := transactionRule("isolated", "isolated", 21000)
	rule.SourceIPMode = SourceIPModeAllowlist
	rule.SourceIPs = []string{"192.0.2.1"}
	if err := manager.StartRule(rule); err != nil {
		t.Fatal(err)
	}

	rule.SourceIPs[0] = "198.51.100.1"
	first := manager.RunningRules()
	if got := first[0].SourceIPs[0]; got != "192.0.2.1" {
		t.Fatalf("caller mutation changed managed policy to %q", got)
	}
	first[0].SourceIPs[0] = "203.0.113.1"
	if got := manager.RunningRules()[0].SourceIPs[0]; got != "192.0.2.1" {
		t.Fatalf("snapshot mutation changed managed policy to %q", got)
	}
}

func TestManagerRestartsOnlyForSemanticSourceIPPolicyChanges(t *testing.T) {
	script := &scriptedRunnerFactory{startErrors: make(map[string][]error)}
	manager := newScriptedManager(NewCollector(), script)
	defer manager.StopAll()
	rule := transactionRule("policy", "policy", 21000)
	rule.SourceIPMode = SourceIPModeAllowlist
	rule.SourceIPs = []string{"192.0.2.0/24", "2001:db8::/32"}
	if err := manager.StartRule(rule); err != nil {
		t.Fatal(err)
	}

	equivalent := rule
	equivalent.SourceIPs = []string{"2001:db8::/32", "192.0.2.42/24", "192.0.2.0/24"}
	result := manager.ApplySnapshot([]Rule{equivalent}, ApplySnapshotOptions{ReplaceAll: true})
	if result.FailedRules != 0 || len(result.Items) != 1 || result.Items[0].Action != ApplyActionUnchanged {
		t.Fatalf("equivalent policy apply = %+v", result)
	}
	if got := append([]string(nil), script.events...); !reflect.DeepEqual(got, []string{"start:policy"}) {
		t.Fatalf("equivalent policy restarted runner: %v", got)
	}

	changed := equivalent
	changed.SourceIPs = []string{"198.51.100.0/24"}
	result = manager.ApplySnapshot([]Rule{changed}, ApplySnapshotOptions{ReplaceAll: true})
	if result.FailedRules != 0 || len(result.Items) != 1 || result.Items[0].Action != ApplyActionRestarted {
		t.Fatalf("changed policy apply = %+v", result)
	}
	if got := script.events; !reflect.DeepEqual(got, []string{"start:policy", "stop:policy", "start:policy"}) {
		t.Fatalf("changed policy lifecycle = %v", got)
	}
}

func TestApplySnapshotTransactionalPrevalidatesEntireBatch(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: make(map[string][]error)}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	oldRule := transactionRule("a", "old-a", 21001)
	if err := manager.StartRule(oldRule); err != nil {
		t.Fatal(err)
	}
	eventsBefore := append([]string(nil), script.events...)

	invalid := transactionRule("b", "", 21002)
	result := manager.ApplySnapshotTransactional([]Rule{
		transactionRule("a", "new-a", 22001),
		invalid,
	}, ApplySnapshotOptions{ReplaceAll: true})
	if result.ApplyFailure == nil || !strings.Contains(result.ApplyFailure.Error, "missing rule name") {
		t.Fatalf("apply failure = %+v, want validation error", result.ApplyFailure)
	}
	if result.Rollback.Attempted {
		t.Fatalf("validation failure unexpectedly attempted rollback: %+v", result.Rollback)
	}
	if !reflect.DeepEqual(script.events, eventsBefore) {
		t.Fatalf("runtime changed before validation completed: %v", script.events)
	}
	if got := manager.RunningRules(); len(got) != 1 || !got[0].RuntimeEqual(oldRule) || got[0].Name != oldRule.Name {
		t.Fatalf("running rules changed after validation failure: %+v", got)
	}
}

func TestApplySnapshotTransactionalUpdateFailureRestoresOldRuleAndCounters(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: map[string][]error{
		"new-a": {errors.New("new runner unavailable")},
	}}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	oldRule := transactionRule("a", "old-a", 21001)
	if err := manager.StartRule(oldRule); err != nil {
		t.Fatal(err)
	}
	collector.AddUpload("a", 11)
	collector.AddDownload("a", 22)
	collector.IncSourceIPDenied("a")
	collector.SetConns("a", 3)
	counterBefore := manager.Snapshot("a")
	protocolsBefore := manager.RuleProtocols()

	result := manager.ApplySnapshotTransactional(
		[]Rule{transactionRule("a", "new-a", 22001)},
		ApplySnapshotOptions{ReplaceAll: true},
	)
	if result.ApplyFailure == nil || result.ApplyFailure.RuleID != "a" {
		t.Fatalf("apply failure = %+v, want rule a", result.ApplyFailure)
	}
	if !result.Rollback.Attempted || result.Rollback.Failed {
		t.Fatalf("rollback = %+v, want successful restoration", result.Rollback)
	}
	if len(result.Rollback.Items) != 1 || result.Rollback.Items[0].RuleID != "a" || result.Rollback.Items[0].Status != "ok" {
		t.Fatalf("rollback items = %+v", result.Rollback.Items)
	}
	if got := manager.RunningRules(); len(got) != 1 || !got[0].RuntimeEqual(oldRule) || got[0].Name != oldRule.Name {
		t.Fatalf("old rule was not restored: %+v", got)
	}
	gotCounter := manager.Snapshot("a")
	if gotCounter.UploadBytes != counterBefore.UploadBytes || gotCounter.DownloadBytes != counterBefore.DownloadBytes || gotCounter.SourceIPDenied != counterBefore.SourceIPDenied || gotCounter.UDPSessionRejected != counterBefore.UDPSessionRejected || gotCounter.UDPPacketsDropped != counterBefore.UDPPacketsDropped {
		t.Fatalf("cumulative counters after rollback = %+v, want values from %+v", gotCounter, counterBefore)
	}
	if gotCounter.Conns != 0 {
		t.Fatalf("connections after runner restart = %d, want 0", gotCounter.Conns)
	}
	if got := manager.RuleProtocols(); !reflect.DeepEqual(got, protocolsBefore) {
		t.Fatalf("protocol metadata after rollback = %+v, want %+v", got, protocolsBefore)
	}
	wantEvents := []string{"start:old-a", "stop:old-a", "start:new-a", "start:old-a"}
	if !reflect.DeepEqual(script.events, wantEvents) {
		t.Fatalf("runner events = %v, want %v", script.events, wantEvents)
	}
}

func TestApplySnapshotTransactionalRollsBackPriorRulesInReverseOrder(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: map[string][]error{
		"new-c": {errors.New("third rule failed")},
	}}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	oldA := transactionRule("a", "old-a", 21001)
	oldB := transactionRule("b", "old-b", 21002)
	if err := manager.StartRule(oldA); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartRule(oldB); err != nil {
		t.Fatal(err)
	}
	eventsBefore := len(script.events)

	result := manager.ApplySnapshotTransactional([]Rule{
		transactionRule("a", "new-a", 22001),
		transactionRule("b", "new-b", 22002),
		transactionRule("c", "new-c", 22003),
	}, ApplySnapshotOptions{ReplaceAll: true})
	if result.ApplyFailure == nil || result.ApplyFailure.RuleID != "c" {
		t.Fatalf("apply failure = %+v, want rule c", result.ApplyFailure)
	}
	if result.Rollback.Failed || len(result.Rollback.Items) != 2 {
		t.Fatalf("rollback = %+v", result.Rollback)
	}
	if got := []string{result.Rollback.Items[0].RuleID, result.Rollback.Items[1].RuleID}; !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("rollback order = %v, want [b a]", got)
	}
	wantEvents := []string{
		"stop:old-a", "start:new-a",
		"stop:old-b", "start:new-b",
		"start:new-c",
		"stop:new-b", "start:old-b",
		"stop:new-a", "start:old-a",
	}
	if got := script.events[eventsBefore:]; !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("transaction events = %v, want %v", got, wantEvents)
	}
	if got := manager.RunningRules(); len(got) != 2 || got[0].Name != "old-a" || got[1].Name != "old-b" {
		t.Fatalf("running rules after rollback = %+v", got)
	}
}

func TestApplySnapshotTransactionalPreservesConnsForMetadataOnlyRollback(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: map[string][]error{
		"new-c": {errors.New("later apply failed")},
	}}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	oldA := transactionRule("a", "old-a", 21001)
	if err := manager.StartRule(oldA); err != nil {
		t.Fatal(err)
	}
	collector.SetConns("a", 4)
	metadataOnly := oldA
	metadataOnly.Name = "renamed-a"
	metadataOnly.Remark = "metadata change"

	result := manager.ApplySnapshotTransactional([]Rule{
		metadataOnly,
		transactionRule("c", "new-c", 22003),
	}, ApplySnapshotOptions{ReplaceAll: true})
	if result.ApplyFailure == nil || result.Rollback.Failed {
		t.Fatalf("transaction result = %+v", result)
	}
	if got := manager.Snapshot("a").Conns; got != 4 {
		t.Fatalf("metadata-only rule connections = %d after rollback, want 4", got)
	}
	if got := manager.RunningRules(); len(got) != 1 || got[0].Name != oldA.Name || got[0].Remark != oldA.Remark {
		t.Fatalf("metadata was not rolled back: %+v", got)
	}
}

func TestPendingSnapshotRollbackPreservesLiveCounterUpdates(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: make(map[string][]error)}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	oldRule := transactionRule("live", "old", 21001)
	if err := manager.StartRule(oldRule); err != nil {
		t.Fatal(err)
	}
	collector.AddUpload("live", 10)
	collector.SetConns("live", 2)
	metadataOnly := oldRule
	metadataOnly.Name = "renamed"

	pending, result := manager.BeginApplySnapshotTransactional([]Rule{metadataOnly}, ApplySnapshotOptions{ReplaceAll: true})
	if pending == nil || result.ApplyFailure != nil {
		t.Fatalf("begin transaction = (%v, %+v)", pending, result)
	}
	// The runner remains live while an external config commit is pending.
	collector.AddUpload("live", 7)
	collector.IncConns("live")
	rollback := pending.Rollback()
	if rollback.Failed {
		t.Fatalf("rollback = %+v", rollback)
	}
	got := manager.Snapshot("live")
	if got.UploadBytes != 17 || got.Conns != 3 {
		t.Fatalf("live counters were rewound by rollback: %+v", got)
	}
	if rules := manager.RunningRules(); len(rules) != 1 || rules[0].Name != oldRule.Name {
		t.Fatalf("metadata was not restored: %+v", rules)
	}
}

func TestPendingSnapshotRollbackRestoresRemovedCountersAndDisabledMetadata(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: make(map[string][]error)}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	running := transactionRule("running", "running", 21001)
	disabled := transactionRule("disabled", "disabled", 21002)
	disabled.Enabled = false
	if result := manager.ApplySnapshot([]Rule{running, disabled}, ApplySnapshotOptions{ReplaceAll: true}); result.FailedRules != 0 {
		t.Fatalf("initial apply = %+v", result)
	}
	collector.AddUpload("running", 11)
	collector.AddDownload("disabled", 22)

	pending, result := manager.BeginApplySnapshotTransactional(nil, ApplySnapshotOptions{ReplaceAll: true})
	if pending == nil || result.ApplyFailure != nil {
		t.Fatalf("begin removal = (%v, %+v)", pending, result)
	}
	if snapshots := manager.SnapshotAll(); len(snapshots) != 0 {
		t.Fatalf("candidate counters were not removed: %+v", snapshots)
	}
	rollback := pending.Rollback()
	if rollback.Failed {
		t.Fatalf("rollback = %+v", rollback)
	}
	if got := manager.Snapshot("running"); got.UploadBytes != 11 || got.Conns != 0 {
		t.Fatalf("running counter was not restored correctly: %+v", got)
	}
	if got := manager.Snapshot("disabled"); got.DownloadBytes != 22 {
		t.Fatalf("disabled counter was not restored: %+v", got)
	}
	protocols := manager.RuleProtocols()
	if protocols["running"] != ProtocolTCP || protocols["disabled"] != ProtocolTCP {
		t.Fatalf("protocol metadata was not restored: %+v", protocols)
	}
}

func TestApplySnapshotTransactionalReportsRollbackFailure(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: map[string][]error{
		"old-a": {nil, errors.New("old runner cannot be restored")},
		"new-c": {errors.New("later apply failed")},
	}}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	oldA := transactionRule("a", "old-a", 21001)
	newA := transactionRule("a", "new-a", 22001)
	newA.Protocol = ProtocolUDP
	if err := manager.StartRule(oldA); err != nil {
		t.Fatal(err)
	}
	result := manager.ApplySnapshotTransactional([]Rule{
		newA,
		transactionRule("c", "new-c", 22003),
	}, ApplySnapshotOptions{ReplaceAll: true})
	if result.ApplyFailure == nil || result.ApplyFailure.RuleID != "c" {
		t.Fatalf("apply failure = %+v, want rule c", result.ApplyFailure)
	}
	if !result.Rollback.Attempted || !result.Rollback.Failed {
		t.Fatalf("rollback failure was not observable: %+v", result.Rollback)
	}
	if len(result.Rollback.Items) != 1 || result.Rollback.Items[0].Status != "failed" || !strings.Contains(result.Rollback.Items[0].Error, "old runner cannot be restored") {
		t.Fatalf("rollback items = %+v", result.Rollback.Items)
	}
	if got := manager.RunningRules(); len(got) != 1 || !got[0].RuntimeEqual(newA) || got[0].Name != newA.Name {
		t.Fatalf("best-effort rollback should retain the last working runner: %+v", got)
	}
	if got := manager.RuleProtocols()["a"]; got != ProtocolUDP {
		t.Fatalf("protocol metadata = %q, want active replacement protocol %q", got, ProtocolUDP)
	}
}

func TestApplySnapshotTransactionalDisabledAndRemove(t *testing.T) {
	collector := NewCollector()
	script := &scriptedRunnerFactory{startErrors: make(map[string][]error)}
	manager := newScriptedManager(collector, script)
	defer manager.StopAll()

	ruleA := transactionRule("a", "old-a", 21001)
	ruleB := transactionRule("b", "old-b", 21002)
	if err := manager.StartRule(ruleA); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartRule(ruleB); err != nil {
		t.Fatal(err)
	}
	disabledA := ruleA
	disabledA.Enabled = false

	result := manager.ApplySnapshotTransactional([]Rule{disabledA}, ApplySnapshotOptions{ReplaceAll: true})
	if result.ApplyFailure != nil || result.Rollback.Attempted {
		t.Fatalf("transaction failed: %+v", result)
	}
	if len(result.Apply.Items) != 2 || result.Apply.Items[0].Action != ApplyActionStopped || result.Apply.Items[1].Action != ApplyActionRemoved {
		t.Fatalf("apply items = %+v, want stopped then removed", result.Apply.Items)
	}
	if manager.RunningCount() != 0 {
		t.Fatalf("running count = %d, want 0", manager.RunningCount())
	}
	if snapshots := manager.SnapshotAll(); len(snapshots) != 1 || snapshots[0].RuleID != "a" {
		t.Fatalf("disabled counter should remain and removed counter should not: %+v", snapshots)
	}
	protocols := manager.RuleProtocols()
	if len(protocols) != 1 || protocols["a"] != ProtocolTCP {
		t.Fatalf("protocol metadata = %+v, want only disabled rule a", protocols)
	}

	removeDisabled := manager.ApplySnapshotTransactional(nil, ApplySnapshotOptions{ReplaceAll: true})
	if removeDisabled.ApplyFailure != nil || len(removeDisabled.Apply.Items) != 1 || removeDisabled.Apply.Items[0].RuleID != "a" || removeDisabled.Apply.Items[0].Action != ApplyActionRemoved {
		t.Fatalf("removing disabled rule = %+v", removeDisabled)
	}
	if snapshots := manager.SnapshotAll(); len(snapshots) != 0 {
		t.Fatalf("removed disabled counter remains: %+v", snapshots)
	}
	if protocols := manager.RuleProtocols(); len(protocols) != 0 {
		t.Fatalf("removed disabled protocol remains: %+v", protocols)
	}
}

func TestApplySnapshotStartAndRemove(t *testing.T) {
	manager := NewManager(NewCollector())
	defer manager.StopAll()

	rule := Rule{
		RuleID:     "rule-1",
		Name:       "rule-1",
		Protocol:   ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: freeTCPPort(t),
		TargetAddr: "127.0.0.1",
		TargetPort: 65535,
		Enabled:    true,
	}

	result := manager.ApplySnapshot([]Rule{rule}, ApplySnapshotOptions{ReplaceAll: true})
	if result.FailedRules != 0 {
		t.Fatalf("expected no failures, got %+v", result)
	}
	if manager.RunningCount() != 1 {
		t.Fatalf("expected 1 running rule, got %d", manager.RunningCount())
	}

	result = manager.ApplySnapshot(nil, ApplySnapshotOptions{ReplaceAll: true})
	if result.StoppedRules != 1 {
		t.Fatalf("expected 1 stopped rule, got %+v", result)
	}
	if manager.RunningCount() != 0 {
		t.Fatalf("expected 0 running rule, got %d", manager.RunningCount())
	}
}

func TestApplySnapshotRejectsDuplicateRuleID(t *testing.T) {
	manager := NewManager(NewCollector())
	defer manager.StopAll()

	port1 := freeTCPPort(t)
	port2 := freeTCPPort(t)
	result := manager.ApplySnapshot([]Rule{
		{
			RuleID:     "dup",
			Name:       "dup-1",
			Protocol:   ProtocolTCP,
			ListenAddr: "127.0.0.1",
			ListenPort: port1,
			TargetAddr: "127.0.0.1",
			TargetPort: 65535,
			Enabled:    true,
		},
		{
			RuleID:     "dup",
			Name:       "dup-2",
			Protocol:   ProtocolTCP,
			ListenAddr: "127.0.0.1",
			ListenPort: port2,
			TargetAddr: "127.0.0.1",
			TargetPort: 65535,
			Enabled:    true,
		},
	}, ApplySnapshotOptions{ReplaceAll: true})
	if result.FailedRules != 1 {
		t.Fatalf("expected 1 failure, got %+v", result)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen temp port failed: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestApplySnapshotConcurrentSafety(t *testing.T) {
	manager := NewManager(NewCollector())
	defer manager.StopAll()

	ports := make([]int, 10)
	for i := range ports {
		ports[i] = freeTCPPort(t)
	}
	rules := make([]Rule, 10)
	for i := range rules {
		rules[i] = Rule{
			RuleID:     fmt.Sprintf("rule-%d", i),
			Name:       fmt.Sprintf("rule-%d", i),
			Protocol:   ProtocolTCP,
			ListenAddr: "127.0.0.1",
			ListenPort: ports[i],
			TargetAddr: "127.0.0.1",
			TargetPort: 65535,
			Enabled:    true,
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.ApplySnapshot(rules, ApplySnapshotOptions{ReplaceAll: true})
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = manager.StartRule(rules[idx])
			manager.StopRule(rules[idx].RuleID)
		}(i)
	}
	wg.Wait()
}
