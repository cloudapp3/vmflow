package engine

import "testing"

func TestUDPSessionBudgetEnforcesAndReleases(t *testing.T) {
	budget := newUDPSessionBudget(2)
	if !budget.tryAcquire() || !budget.tryAcquire() {
		t.Fatal("expected first two sessions to be admitted")
	}
	if budget.tryAcquire() {
		t.Fatal("expected third session to be rejected")
	}
	budget.release()
	if !budget.tryAcquire() {
		t.Fatal("expected released capacity to be reusable")
	}
	limit, active := budget.snapshot()
	if limit != 2 || active != 2 {
		t.Fatalf("snapshot = (%d, %d), want (2, 2)", limit, active)
	}
}

func TestUDPSessionBudgetLimitCanBeLoweredWithoutDroppingActive(t *testing.T) {
	budget := newUDPSessionBudget(2)
	if !budget.tryAcquire() || !budget.tryAcquire() {
		t.Fatal("expected sessions to be admitted")
	}
	budget.setLimit(1)
	if budget.tryAcquire() {
		t.Fatal("expected admission to remain blocked above lowered limit")
	}
	budget.release()
	if budget.tryAcquire() {
		t.Fatal("expected admission to remain blocked at lowered limit")
	}
	budget.release()
	if !budget.tryAcquire() {
		t.Fatal("expected admission below lowered limit")
	}
}

func TestManagerUDPMaxSessionsUsesSafeDefault(t *testing.T) {
	manager := NewManagerWithOptions(nil, ManagerOptions{})
	limit, active := manager.UDPMaxSessions()
	if limit != DefaultUDPGlobalMaxSessions || active != 0 {
		t.Fatalf("UDPMaxSessions = (%d, %d), want (%d, 0)", limit, active, DefaultUDPGlobalMaxSessions)
	}
}

func TestManagerSharesUDPBudgetAcrossRules(t *testing.T) {
	manager := NewManagerWithOptions(nil, ManagerOptions{UDPMaxSessions: 1})
	first, err := manager.buildRunners(Rule{Protocol: ProtocolUDP})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.buildRunners(Rule{Protocol: ProtocolTCPUDP})
	if err != nil {
		t.Fatal(err)
	}
	firstUDP := first[0].(*udpRunner)
	secondUDP := second[1].(*udpRunner)
	if firstUDP.budget != manager.udpBudget || secondUDP.budget != manager.udpBudget {
		t.Fatal("UDP runners did not receive the manager-wide budget")
	}
	if !firstUDP.budget.tryAcquire() {
		t.Fatal("first rule did not acquire shared capacity")
	}
	if secondUDP.budget.tryAcquire() {
		t.Fatal("second rule bypassed the shared capacity")
	}
	firstUDP.budget.release()
}
