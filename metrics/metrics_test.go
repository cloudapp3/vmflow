package metrics

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/engine"
)

func TestWriteMetrics(t *testing.T) {
	traffic := engine.NewCollector()
	manager := engine.NewManager(traffic)
	defer manager.StopAll()
	result := manager.ApplySnapshot([]engine.Rule{{
		RuleID:     "disabled",
		Name:       "disabled",
		Protocol:   engine.ProtocolUDP,
		ListenAddr: "127.0.0.1",
		ListenPort: 1,
		TargetAddr: "127.0.0.1",
		TargetPort: 2,
		Enabled:    false,
	}}, engine.ApplySnapshotOptions{ReplaceAll: true})

	collector := New(manager)
	collector.ObserveControlRequest("GET", "/v1/rules", 200, 10*time.Millisecond)
	collector.ObserveReload("ok")
	collector.ObserveApplyResult(result)
	traffic.IncUDPSessionRejected("disabled")
	traffic.IncUDPPacketsDropped("disabled")

	var b strings.Builder
	if err := collector.Write(&b); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"vmflow_uptime_seconds",
		"vmflow_udp_sessions_limit 256",
		"vmflow_udp_sessions_active 0",
		`vmflow_rule_connections{rule_id="disabled",protocol="udp"} 0`,
		`vmflow_udp_session_rejected_total{rule_id="disabled",protocol="udp"} 1`,
		`vmflow_udp_packets_dropped_total{rule_id="disabled",protocol="udp"} 1`,
		`vmflow_control_requests_total{method="GET",path="/v1/rules",status="200"} 1`,
		`vmflow_reload_total{status="ok"} 1`,
		`vmflow_rule_apply_total{action="unchanged",status="ok"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, out)
		}
	}
}

func TestObserveControlRequestNormalizesLabels(t *testing.T) {
	collector := New(nil)
	for i := 0; i < 100; i++ {
		collector.ObserveControlRequest("custom-method", fmt.Sprintf("/untrusted/%d", i), 404, time.Millisecond)
	}
	collector.ObserveControlRequest(" get ", "/v1/certs/example.com", 200, time.Millisecond)
	collector.ObserveControlRequest("DELETE", "/v1/certs/another.example", 204, time.Millisecond)

	requests, _, _ := collector.copyCounters()
	if len(requests) != 3 {
		t.Fatalf("expected 3 bounded label combinations, got %d: %#v", len(requests), requests)
	}
	unknown := requests[controlRequestKey{Method: "OTHER", Path: "/unknown", Status: "404"}]
	if unknown.Count != 100 {
		t.Fatalf("expected unknown requests to share one label, got %+v", unknown)
	}
	for _, key := range []controlRequestKey{
		{Method: "GET", Path: "/v1/certs/{domain}", Status: "200"},
		{Method: "DELETE", Path: "/v1/certs/{domain}", Status: "204"},
	} {
		if requests[key].Count != 1 {
			t.Fatalf("expected normalized request key %+v, got %#v", key, requests)
		}
	}
}

func TestNormalizeControlPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "fixed", path: "/v1/precheck", want: "/v1/precheck"},
		{name: "cert detail", path: "/v1/certs/example.com", want: "/v1/certs/{domain}"},
		{name: "cert obtain", path: "/v1/certs/obtain", want: "/v1/certs/obtain"},
		{name: "nested unknown", path: "/v1/certs/example.com/private", want: "/unknown"},
		{name: "empty", path: "", want: "/unknown"},
		{name: "unknown", path: "/v1/attacker", want: "/unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeControlPath(tt.path); got != tt.want {
				t.Fatalf("normalizeControlPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestHasUDPStats(t *testing.T) {
	if !hasUDPStats("udp", engine.TrafficSnapshot{}) || !hasUDPStats("tcp+udp", engine.TrafficSnapshot{}) {
		t.Fatal("UDP-capable protocols must expose zero-valued protection counters")
	}
	if hasUDPStats("tcp", engine.TrafficSnapshot{}) {
		t.Fatal("TCP-only rules must not add zero-valued UDP metric series")
	}
	if !hasUDPStats("unknown", engine.TrafficSnapshot{UDPPacketsDropped: 1}) {
		t.Fatal("stopped rules with UDP protection events must remain visible")
	}
}

func BenchmarkProtocolIndexAndLookup10000(b *testing.B) {
	rules := make([]engine.Rule, 10_000)
	for i := range rules {
		rules[i] = engine.Rule{RuleID: fmt.Sprintf("rule-%d", i), Protocol: engine.ProtocolTCP}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		protocols := indexRuleProtocols(rules)
		for _, rule := range rules {
			if protocolForRule(protocols, rule.RuleID) == "unknown" {
				b.Fatal("indexed rule not found")
			}
		}
	}
}
