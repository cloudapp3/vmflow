package precheck

import (
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
