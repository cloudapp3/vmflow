package engine

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

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
