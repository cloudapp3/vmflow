package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/engine"
)

func TestWriteMetrics(t *testing.T) {
	manager := engine.NewManager(engine.NewCollector())
	defer manager.StopAll()
	result := manager.ApplySnapshot([]engine.Rule{{
		RuleID:     "disabled",
		Name:       "disabled",
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: 1,
		TargetAddr: "127.0.0.1",
		TargetPort: 2,
		Enabled:    false,
	}}, engine.ApplySnapshotOptions{ReplaceAll: true})

	collector := New(manager)
	collector.ObserveAdminRequest("GET", "/healthz", 200, 10*time.Millisecond)
	collector.ObserveReload("ok")
	collector.ObserveApplyResult(result)

	var b strings.Builder
	if err := collector.Write(&b); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"vmflow_uptime_seconds",
		`vmflow_rule_connections{rule_id="disabled",protocol="unknown"} 0`,
		`vmflow_admin_requests_total{method="GET",path="/healthz",status="200"} 1`,
		`vmflow_reload_total{status="ok"} 1`,
		`vmflow_rule_apply_total{action="unchanged",status="ok"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, out)
		}
	}
}
