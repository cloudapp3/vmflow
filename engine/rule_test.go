package engine

import "testing"

func TestRuntimeEqualUsesEffectiveUDPDefaultLimit(t *testing.T) {
	base := Rule{Protocol: ProtocolUDP, MaxConn: 0}
	explicit := base
	explicit.MaxConn = DefaultUDPRuleMaxSessions
	if !base.RuntimeEqual(explicit) {
		t.Fatal("equivalent UDP default and explicit limits should not restart a rule")
	}

	tcpBase := Rule{Protocol: ProtocolTCP, MaxConn: 0}
	tcpExplicit := tcpBase
	tcpExplicit.MaxConn = DefaultUDPRuleMaxSessions
	if tcpBase.RuntimeEqual(tcpExplicit) {
		t.Fatal("TCP zero remains unlimited and must not equal an explicit limit")
	}

	combinedBase := Rule{Protocol: ProtocolTCPUDP, MaxConn: 0}
	combinedExplicit := combinedBase
	combinedExplicit.MaxConn = DefaultUDPRuleMaxSessions
	if combinedBase.RuntimeEqual(combinedExplicit) {
		t.Fatal("tcp+udp zero keeps unlimited TCP semantics and must restart")
	}
}
