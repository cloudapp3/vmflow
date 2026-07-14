package tui

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudapp3/vmflow/precheck"
)

func TestPendingOperationKeyGuardBlocksLeavingAndMutating(t *testing.T) {
	operations := []operationState{operationPrechecking, operationApplying, operationReloading}
	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyTab},
		{Type: tea.KeyRunes, Runes: []rune{'u'}},
		{Type: tea.KeyRunes, Runes: []rune{'e'}},
		{Type: tea.KeyEnter},
	}

	for _, operation := range operations {
		for _, key := range keys {
			m := managedTestModel()
			m.operation = operation
			m.view = viewPrecheck
			m.overlay = overlayReload
			m.stageToggle()
			originalDraft := m.draft

			updated, cmd := m.handleKey(key)
			got := updated.(Model)
			if cmd != nil {
				t.Fatalf("operation=%v key=%q cmd=%v", operation, key.String(), cmd)
			}
			if got.view != viewPrecheck || got.overlay != overlayReload || got.draft != originalDraft {
				t.Fatalf("operation=%v key=%q changed pending state: view=%v overlay=%v draft=%p", operation, key.String(), got.view, got.overlay, got.draft)
			}
			if !strings.Contains(got.statusText, "in progress") {
				t.Fatalf("operation=%v key=%q status=%q", operation, key.String(), got.statusText)
			}
		}
	}

	m := managedTestModel()
	m.operation = operationApplying
	_, _, handled := m.handlePendingOperationKey(tea.KeyMsg{Type: tea.KeyDown})
	if handled {
		t.Fatal("selection navigation should remain available while an operation is pending")
	}
}

func TestPrecheckFindingEditHonorsStaleBarrier(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncStale
	m.remoteRevision = "rev-2"
	m.view = viewPrecheck
	m.precheckResult = &PrecheckResponse{Precheck: precheck.Result{Items: []precheck.Item{{
		Severity: precheck.SeverityError,
		RuleID:   "running",
		Check:    "revision",
		Message:  "configuration changed",
	}}}}

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = updated.(Model)
	if cmd != nil || m.editor != nil || m.view != viewPrecheck {
		t.Fatalf("stale precheck opened editor: view=%v editor=%+v cmd=%v", m.view, m.editor, cmd)
	}
	if !strings.Contains(m.statusText, "stale") {
		t.Fatalf("stale precheck status=%q", m.statusText)
	}
}

func TestRulesApplyCommitted503ClearsDraftAndReconciles(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	m.operation = operationApplying
	previousRequestID := m.configRequestID

	updated, cmd := m.handleApplyResult(applyMsg{err: &APIError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "configuration committed but durability could not be confirmed",
		Body:       []byte(`{"error":"configuration committed but durability could not be confirmed","committed":true,"revision":"rev-2"}`),
	}})
	m = updated.(Model)
	if cmd == nil || m.configRequestID != previousRequestID+1 || m.configBarrierID != m.configRequestID {
		t.Fatalf("committed result did not start reconciliation: request=%d barrier=%d cmd=%v", m.configRequestID, m.configBarrierID, cmd)
	}
	if m.draft != nil || m.sync != syncUnavailable || m.operation != operationIdle || m.view != viewRules {
		t.Fatalf("committed result state: draft=%+v sync=%v operation=%v view=%v", m.draft, m.sync, m.operation, m.view)
	}
	if !strings.Contains(m.statusText, "committed") || !strings.Contains(m.statusText, "durability") {
		t.Fatalf("committed status=%q", m.statusText)
	}

	updated, _ = m.Update(configRulesMsg{
		requestID: m.configBarrierID,
		resp: &ConfigRulesResponse{
			Revision: "rev-2", ETag: `"rev-2"`, Writable: true, UDPMaxSessions: 256,
			Rules: cloneRules(m.config.Rules),
		},
	})
	m = updated.(Model)
	if m.configBarrierID != 0 || m.sync != syncClean || m.config == nil || m.config.Revision != "rev-2" {
		t.Fatalf("committed reconciliation did not converge: barrier=%d sync=%v config=%+v", m.configBarrierID, m.sync, m.config)
	}
}

func TestRulesApplyUncertainRollbackRetainsDraftBehindBarrier(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	m.operation = operationApplying
	draft := m.draft

	updated, cmd := m.handleApplyResult(applyMsg{err: &APIError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "configuration could not be committed",
		Body:       []byte(`{"error":"configuration could not be committed","rollback_ok":false}`),
	}})
	m = updated.(Model)
	if cmd == nil || m.configBarrierID == 0 || m.sync != syncStale || m.draft != draft {
		t.Fatalf("uncertain rollback state: cmd=%v barrier=%d sync=%v draft=%p", cmd, m.configBarrierID, m.sync, m.draft)
	}
	if m.prepareWrite() || !strings.Contains(m.statusText, "refresh") {
		t.Fatalf("write was allowed before reconciliation: status=%q", m.statusText)
	}

	requestID := m.configBarrierID
	updated, _ = m.Update(configRulesMsg{
		requestID: requestID,
		resp: &ConfigRulesResponse{
			Revision: "rev-1", ETag: `"rev-1"`, Writable: true, UDPMaxSessions: 256,
			Rules: cloneRules(m.config.Rules),
		},
	})
	m = updated.(Model)
	if m.configBarrierID != 0 || m.sync != syncDirty || m.draft != draft || m.precheckResult != nil {
		t.Fatalf("same-revision reconciliation state: barrier=%d sync=%v draft=%p precheck=%+v", m.configBarrierID, m.sync, m.draft, m.precheckResult)
	}
	updated, cmd = m.handleManagementKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	m = updated.(Model)
	if cmd != nil || m.overlay == overlayApply || !strings.Contains(m.statusText, "precheck") {
		t.Fatalf("old ETag could be retried without precheck: overlay=%v status=%q cmd=%v", m.overlay, m.statusText, cmd)
	}
}

func TestRulesApplyNetworkFailureStartsReconciliation(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	m.operation = operationApplying
	draft := m.draft

	updated, cmd := m.handleApplyResult(applyMsg{err: errors.New("request timeout")})
	m = updated.(Model)
	if cmd == nil || m.configBarrierID == 0 || m.sync != syncStale || m.draft != draft {
		t.Fatalf("network failure state: cmd=%v barrier=%d sync=%v draft=%p", cmd, m.configBarrierID, m.sync, m.draft)
	}
	if !strings.Contains(m.statusText, "uncertain") || !strings.Contains(m.statusText, "reconciling") {
		t.Fatalf("network failure status=%q", m.statusText)
	}
}

func TestRulesApplyConfirmedRollbackDoesNotCreateBarrier(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	m.operation = operationApplying
	draft := m.draft

	updated, cmd := m.handleApplyResult(applyMsg{err: &APIError{
		StatusCode: http.StatusInternalServerError,
		Message:    "configuration could not be committed",
		Body:       []byte(`{"error":"configuration could not be committed","rollback_ok":true}`),
	}})
	m = updated.(Model)
	if cmd != nil || m.configBarrierID != 0 || m.sync != syncDirty || m.draft != draft {
		t.Fatalf("confirmed rollback state: cmd=%v barrier=%d sync=%v draft=%p", cmd, m.configBarrierID, m.sync, m.draft)
	}
	if !strings.Contains(m.statusText, "runtime rolled back") {
		t.Fatalf("confirmed rollback status=%q", m.statusText)
	}
}

func TestDiscardingStaleDraftWaitsForFreshConfig(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncStale
	m.remoteRevision = "rev-2"
	m.overlay = overlayDiscardDraft

	updated, cmd := m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.draft != nil || m.configBarrierID == 0 || m.sync != syncUnavailable {
		t.Fatalf("stale discard state: cmd=%v draft=%+v barrier=%d sync=%v", cmd, m.draft, m.configBarrierID, m.sync)
	}
	if m.prepareWrite() || !strings.Contains(m.statusText, "refresh") {
		t.Fatalf("write was allowed during stale discard refresh: status=%q", m.statusText)
	}

	requestID := m.configBarrierID
	updated, _ = m.Update(configRulesMsg{
		requestID: requestID,
		resp: &ConfigRulesResponse{
			Revision: "rev-2", ETag: `"rev-2"`, Writable: true, UDPMaxSessions: 256,
			Rules: cloneRules(m.config.Rules),
		},
	})
	m = updated.(Model)
	if m.configBarrierID != 0 || m.sync != syncClean || !m.prepareWrite() {
		t.Fatalf("fresh config did not release stale discard barrier: barrier=%d sync=%v status=%q", m.configBarrierID, m.sync, m.statusText)
	}
}
