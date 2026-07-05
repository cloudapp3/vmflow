package vmflow

import (
	"errors"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
)

func TestRuntimeApplyAndClose(t *testing.T) {
	rt := New()
	defer rt.Close()

	result := rt.Apply([]engine.Rule{{
		RuleID:     "disabled-rule",
		Name:       "disabled-rule",
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: 18080,
		TargetAddr: "127.0.0.1",
		TargetPort: 18081,
		Enabled:    false,
	}})
	if result.FailedRules != 0 {
		t.Fatalf("expected no failed rules, got %+v", result)
	}
	if rt.RunningCount() != 0 {
		t.Fatalf("expected no running rules, got %d", rt.RunningCount())
	}
	if len(rt.SnapshotAll()) != 1 {
		t.Fatalf("expected disabled rule counter to exist")
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := rt.StartRule(engine.Rule{}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("expected ErrRuntimeClosed, got %v", err)
	}
	closedResult := rt.Apply(nil)
	if closedResult.FailedRules != 0 || closedResult.TotalRules != 0 || len(closedResult.Items) != 0 {
		t.Fatalf("unexpected closed empty apply result: %+v", closedResult)
	}
	closedResult = rt.Apply([]engine.Rule{{RuleID: "x", Name: "x", Protocol: engine.ProtocolTCP, ListenPort: 1, TargetAddr: "127.0.0.1", TargetPort: 2}})
	if closedResult.FailedRules != 1 || closedResult.TotalRules != 1 || len(closedResult.Items) != 1 {
		t.Fatalf("unexpected closed apply result: %+v", closedResult)
	}
}

func TestRuntimeWithCollector(t *testing.T) {
	collector := engine.NewCollector()
	rt := NewRuntime(Options{Collector: collector})
	defer rt.Close()

	if rt.Collector() != collector {
		t.Fatalf("expected supplied collector")
	}
	if rt.Manager() == nil {
		t.Fatalf("expected manager")
	}
}
