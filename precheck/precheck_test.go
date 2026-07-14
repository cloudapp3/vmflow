package precheck

import (
	"fmt"
	"net"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
)

func TestCheckConfigOK(t *testing.T) {
	cfg := config.File{Rules: []engine.Rule{{
		RuleID:     "r1",
		Name:       "r1",
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: 2201,
		TargetAddr: "127.0.0.1",
		TargetPort: 22,
		Enabled:    true,
	}}}
	result := CheckConfig(cfg, nil, Options{CheckTargetResolve: true})
	if !result.OK || result.ErrorCount != 0 {
		t.Fatalf("expected ok, got %+v", result)
	}
}

func TestCheckConfigListenConflict(t *testing.T) {
	port := 2201
	cfg := config.File{Rules: []engine.Rule{
		{
			RuleID:     "r1",
			Name:       "r1",
			Protocol:   engine.ProtocolTCP,
			ListenAddr: "127.0.0.1",
			ListenPort: port,
			TargetAddr: "127.0.0.1",
			TargetPort: 22,
			Enabled:    true,
		},
		{
			RuleID:     "r2",
			Name:       "r2",
			Protocol:   engine.ProtocolTCP,
			ListenAddr: "0.0.0.0",
			ListenPort: port,
			TargetAddr: "127.0.0.1",
			TargetPort: 22,
			Enabled:    true,
		},
	}}
	result := CheckConfig(cfg, nil, Options{})
	if result.OK || result.ErrorCount == 0 {
		t.Fatalf("expected conflict error, got %+v", result)
	}
}

func TestCheckConfigEndpointConflictMatrix(t *testing.T) {
	tests := []struct {
		name      string
		protocol1 engine.Protocol
		addr1     string
		port1     int
		protocol2 engine.Protocol
		addr2     string
		port2     int
		conflict  bool
	}{
		{name: "same specific", protocol1: engine.ProtocolTCP, addr1: "127.0.0.1", port1: 2201, protocol2: engine.ProtocolTCP, addr2: "127.0.0.1", port2: 2201, conflict: true},
		{name: "ipv4 wildcard then specific", protocol1: engine.ProtocolTCP, addr1: "0.0.0.0", port1: 2201, protocol2: engine.ProtocolTCP, addr2: "127.0.0.1", port2: 2201, conflict: true},
		{name: "specific then ipv6 wildcard", protocol1: engine.ProtocolTCP, addr1: "127.0.0.1", port1: 2201, protocol2: engine.ProtocolTCP, addr2: "::", port2: 2201, conflict: true},
		{name: "wildcards conflict", protocol1: engine.ProtocolTCP, addr1: "0.0.0.0", port1: 2201, protocol2: engine.ProtocolTCP, addr2: "::", port2: 2201, conflict: true},
		{name: "different specific", protocol1: engine.ProtocolTCP, addr1: "127.0.0.1", port1: 2201, protocol2: engine.ProtocolTCP, addr2: "127.0.0.2", port2: 2201},
		{name: "tcp and udp isolated", protocol1: engine.ProtocolTCP, addr1: "0.0.0.0", port1: 2201, protocol2: engine.ProtocolUDP, addr2: "127.0.0.1", port2: 2201},
		{name: "different ports", protocol1: engine.ProtocolTCP, addr1: "0.0.0.0", port1: 2201, protocol2: engine.ProtocolTCP, addr2: "127.0.0.1", port2: 2202},
		{name: "tcp udp claims udp", protocol1: engine.ProtocolTCPUDP, addr1: "127.0.0.1", port1: 2201, protocol2: engine.ProtocolUDP, addr2: "0.0.0.0", port2: 2201, conflict: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.File{Rules: []engine.Rule{
				testForwardRule("r1", tt.protocol1, tt.addr1, tt.port1),
				testForwardRule("r2", tt.protocol2, tt.addr2, tt.port2),
			}}
			result := CheckConfig(cfg, nil, Options{})
			gotConflict := hasCheck(result.Items, "listen_conflict")
			if gotConflict != tt.conflict {
				t.Fatalf("listen conflict = %v, want %v; result: %+v", gotConflict, tt.conflict, result)
			}
		})
	}
}

func TestCheckConfigDuplicateEndpointsReportLinearErrors(t *testing.T) {
	rules := make([]engine.Rule, 100)
	for i := range rules {
		rules[i] = testForwardRule(fmt.Sprintf("r-%d", i), engine.ProtocolTCP, "0.0.0.0", 2201)
	}
	result := CheckConfig(config.File{Rules: rules}, nil, Options{})
	if result.ErrorCount != len(rules)-1 {
		t.Fatalf("expected one conflict per duplicate endpoint, got %d errors", result.ErrorCount)
	}
}

func TestCheckConfigBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("listen unavailable in this environment: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := config.File{Rules: []engine.Rule{{
		RuleID:     "r1",
		Name:       "r1",
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: port,
		TargetAddr: "127.0.0.1",
		TargetPort: 22,
		Enabled:    true,
	}}}
	result := CheckConfig(cfg, nil, Options{CheckBind: true})
	if result.OK || result.ErrorCount == 0 {
		t.Fatalf("expected bind error, got %+v", result)
	}
}

func TestCheckConfigSkipsBindForSameRunningRule(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("listen unavailable in this environment: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rule := engine.Rule{
		RuleID:     "r1",
		Name:       "r1",
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: port,
		TargetAddr: "127.0.0.1",
		TargetPort: 22,
		Enabled:    true,
	}
	cfg := config.File{Rules: []engine.Rule{rule}}
	result := CheckConfig(cfg, []engine.Rule{rule}, Options{CheckBind: true})
	if !result.OK {
		t.Fatalf("expected ok because same running rule owns port, got %+v", result)
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

func TestCheckConfigACMEConflict(t *testing.T) {
	cfg := config.File{
		AcmeHTTP01Addr: "127.0.0.1:8080",
		Rules: []engine.Rule{{
			RuleID:     "https",
			Name:       "https",
			Protocol:   engine.ProtocolHTTPS,
			ListenAddr: "0.0.0.0",
			ListenPort: 8080,
			TargetAddr: "127.0.0.1",
			TargetPort: 8081,
			Domains:    []string{"example.com"},
			Enabled:    true,
		}},
	}
	result := CheckConfig(cfg, nil, Options{})
	if result.OK || result.ErrorCount == 0 {
		t.Fatalf("expected acme conflict, got %+v", result)
	}
}

func testForwardRule(id string, protocol engine.Protocol, listenAddr string, listenPort int) engine.Rule {
	return engine.Rule{
		RuleID:     id,
		Name:       id,
		Protocol:   protocol,
		ListenAddr: listenAddr,
		ListenPort: listenPort,
		TargetAddr: "127.0.0.1",
		TargetPort: 22,
		Enabled:    true,
	}
}

func hasCheck(items []Item, check string) bool {
	for _, item := range items {
		if item.Check == check {
			return true
		}
	}
	return false
}

func BenchmarkCheckConfig10000Rules(b *testing.B) {
	rules := make([]engine.Rule, 10_000)
	for i := range rules {
		rules[i] = testForwardRule(
			fmt.Sprintf("r-%d", i),
			engine.ProtocolTCP,
			fmt.Sprintf("10.%d.%d.%d", (i>>16)&255, (i>>8)&255, i&255),
			2201,
		)
	}
	cfg := config.File{Rules: rules}
	running := append([]engine.Rule(nil), rules...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := CheckConfig(cfg, running, Options{CheckBind: true})
		if !result.OK {
			b.Fatalf("precheck failed: %+v", result)
		}
	}
}
