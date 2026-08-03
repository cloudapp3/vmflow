package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/precheck"
)

func TestConfiguredRulesIncludeDisabledAndKeepSelectionByID(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "disabled"
	m.acceptConfig(&ConfigRulesResponse{
		Revision: "rev-1", ETag: `"rev-1"`, Writable: true, UDPMaxSessions: 256,
		Rules: []RuleInfo{testRule("disabled", false, 2202), testRule("running", true, 2201)},
	})
	if got := m.selectedRule(); got == nil || got.RuleID != "disabled" {
		t.Fatalf("selected after reorder = %+v", got)
	}
	m.view = viewRules
	view := m.View()
	if !strings.Contains(view, "DISABLED") || !strings.Contains(view, "disabled") {
		t.Fatalf("rules view omitted disabled rule:\n%s", view)
	}
}

func TestViewerIsReadOnly(t *testing.T) {
	m := managedTestModel()
	m.session = &SessionResponse{Actor: "viewer", Role: "viewer"}
	m.view = viewRules
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	result := updated.(Model)
	if result.editor != nil || result.view == viewEditor {
		t.Fatal("viewer entered rule editor")
	}
	if !strings.Contains(strings.ToLower(result.statusText), "read-only") {
		t.Fatalf("status = %q", result.statusText)
	}
}

func TestCopyToggleDeleteDraftWorkflow(t *testing.T) {
	m := managedTestModel()
	m.view = viewRules
	m.selectedRuleID = "running"

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = updated.(Model)
	if m.view != viewEditor || m.editor == nil || m.editor.kind != editorCopy {
		t.Fatalf("copy did not open editor: view=%v editor=%+v", m.view, m.editor)
	}
	copy, valid := m.editor.rule()
	if !valid {
		t.Fatalf("copy editor invalid: %+v", m.editor.errors)
	}
	if copy.RuleID != "running-copy" || copy.Revision != 0 || copy.CreatedTime != 0 || copy.UpdatedTime != 0 {
		t.Fatalf("copied rule metadata = %+v", copy)
	}
	m.stageEditorRule(copy)
	if m.draft == nil || findRule(m.draft.Rules, "running-copy") == nil || m.sync != syncDirty {
		t.Fatalf("copy was not staged: %+v", m.draft)
	}

	m.selectedRuleID = "running"
	m.stageToggle()
	if rule := findRule(m.draft.Rules, "running"); rule == nil || rule.Enabled {
		t.Fatalf("toggle was not staged: %+v", rule)
	}
	m.stageDelete()
	if findRule(m.draft.Rules, "running") != nil || !m.isDeleted("running") {
		t.Fatalf("delete did not create tombstone: %+v", m.draft)
	}
	if findRule(m.draft.request().Rules, "running") != nil {
		t.Fatal("deleted rule leaked into apply request")
	}
	if !strings.Contains(m.ruleChange("running"), "DELETE") {
		t.Fatalf("change = %q", m.ruleChange("running"))
	}
}

func TestPrecheckApplyAndStaleRetainDraft(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	baseDraft := cloneRules(m.draft.Rules)

	updated, _ := m.handlePrecheckResult(precheckMsg{resp: &PrecheckResponse{
		Revision: m.draft.BaseRevision,
		Diff:     []ConfigRuleDiff{{RuleID: "running", ConfigAction: "update", RuntimeAction: "stop"}},
		Precheck: precheck.Result{OK: true, CheckedRules: len(m.draft.Rules), Items: []precheck.Item{}},
	}})
	m = updated.(Model)
	if m.sync != syncValidated || m.view != viewPrecheck {
		t.Fatalf("precheck state = %v view = %v", m.sync, m.view)
	}
	m.overlay = overlayApply
	updated, cmd := m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.operation != operationApplying || m.draft == nil {
		t.Fatalf("apply not started: operation=%v draft=%+v", m.operation, m.draft)
	}

	updated, _ = m.handleApplyResult(applyMsg{err: &APIError{StatusCode: http.StatusPreconditionFailed, Message: "stale"}})
	m = updated.(Model)
	if m.sync != syncStale || m.draft == nil || !reflectRulesEqual(m.draft.Rules, baseDraft) {
		t.Fatalf("stale apply lost draft: state=%v draft=%+v", m.sync, m.draft)
	}

	m.sync = syncValidated
	m.config.Writable = true
	updated, _ = m.handleApplyResult(applyMsg{resp: &ApplyResponse{
		Revision: "rev-2", UDPMaxSessions: 256, Rules: cloneRules(baseDraft),
		Result: engine.ApplyResult{AppliedRules: 1, TotalRules: len(baseDraft)},
	}})
	m = updated.(Model)
	if m.draft != nil || m.sync != syncClean || m.view != viewApplyResult {
		t.Fatalf("successful apply state = %v draft=%+v view=%v", m.sync, m.draft, m.view)
	}
	if m.config == nil || !m.config.Writable {
		t.Fatalf("apply response without writable did not inherit capability: %+v", m.config)
	}
}

func TestApplyStatusSeparatesConfigAndRuntimeChanges(t *testing.T) {
	m := managedTestModelWithTranslator(i18n.New(i18n.SimplifiedChinese))
	m.selectedRuleID = "running"
	m.stageToggle()
	draft := cloneRules(m.draft.Rules)

	updated, _ := m.handleApplyResult(applyMsg{resp: &ApplyResponse{
		Revision: "rev-2", UDPMaxSessions: 256, Rules: draft,
		Result: engine.ApplyResult{TotalRules: len(draft)},
	}})
	m = updated.(Model)
	if !strings.Contains(m.statusText, "配置已保存：1 项配置更改，0 条运行变更") {
		t.Fatalf("apply status did not distinguish persisted and runtime changes: %q", m.statusText)
	}
}

func TestLanguageSelectorIsAvailableOnNormalViews(t *testing.T) {
	for _, view := range []viewMode{
		viewDashboard,
		viewRules,
		viewDetail,
		viewPrecheck,
		viewApplyResult,
		viewBotConfig,
	} {
		t.Run(fmt.Sprintf("view_%d", view), func(t *testing.T) {
			m := managedTestModel()
			m.view = view
			updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			m = updated.(Model)
			if cmd != nil || m.overlay != overlayLanguage {
				t.Fatalf("language selector did not open: overlay=%v cmd=%v", m.overlay, cmd)
			}
		})
	}
}

func TestLanguageSelectorSwitchesBothDirectionsAndCancels(t *testing.T) {
	m := managedTestModel()
	m.filterInput.SetValue("running")
	m.udpInput.SetValue("256")
	m.udpInputError = m.tf("tui.validation.udp_sessions", engine.MaxUDPGlobalMaxSessions)

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	if output := ansi.Strip(m.renderOverlay()); !strings.Contains(output, "this session only") || !strings.Contains(output, "current") {
		t.Fatalf("language selector did not explain its scope or current choice:\n%s", output)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = updated.(Model)
	if m.tr.Locale() != i18n.SimplifiedChinese || m.overlay != overlayNone {
		t.Fatalf("Chinese selection failed: locale=%q overlay=%v", m.tr.Locale(), m.overlay)
	}
	if m.filterInput.Value() != "running" || m.udpInput.Value() != "256" {
		t.Fatalf("language switch changed input values: filter=%q udp=%q", m.filterInput.Value(), m.udpInput.Value())
	}
	if m.filterInput.Placeholder != "按名称或 ID 筛选..." || m.udpInput.Placeholder != "全局 UDP 会话数" {
		t.Fatalf("input placeholders were not localized: filter=%q udp=%q", m.filterInput.Placeholder, m.udpInput.Placeholder)
	}
	if !strings.Contains(m.udpInputError, "0-") || !strings.Contains(m.statusText, "语言已切换为简体中文") {
		t.Fatalf("localized validation/status missing: udp=%q status=%q", m.udpInputError, m.statusText)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.tr.Locale() != i18n.SimplifiedChinese || m.overlay != overlayNone {
		t.Fatalf("cancel changed language: locale=%q overlay=%v", m.tr.Locale(), m.overlay)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = updated.(Model)
	if m.tr.Locale() != i18n.English || !strings.Contains(m.statusText, "Language changed to English") {
		t.Fatalf("English selection failed: locale=%q status=%q", m.tr.Locale(), m.statusText)
	}
}

func TestLanguageSwitchPreservesRuleEditorState(t *testing.T) {
	m := managedTestModel()
	m.view = viewEditor
	m.editor = newRuleEditorWithTranslator(editorEdit, testRule("running", true, 2201), m.config.Rules, m.tr)
	m.editor.toggleAdvanced()
	if !m.editor.focusField(fieldMaxConn) {
		t.Fatal("could not focus max connections")
	}
	focus := m.editor.focus
	m.editor.fields[focus].input.SetValue("invalid")
	if _, valid := m.editor.rule(); valid || m.editor.errors[fieldMaxConn] != "must be an integer" {
		t.Fatalf("English validation fixture failed: %+v", m.editor.errors)
	}
	editor := m.editor

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyF1})
	m = updated.(Model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	if m.overlay != overlayLanguage {
		t.Fatalf("help did not open language selector: overlay=%v", m.overlay)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = updated.(Model)

	if m.editor != editor || m.editor.focus != focus || !m.editor.advanced || !m.editor.dirty() {
		t.Fatalf("language switch rebuilt or reset editor: %+v", m.editor)
	}
	if !m.editor.fields[focus].input.Focused() || m.editor.value(fieldMaxConn) != "invalid" {
		t.Fatalf("language switch lost editor focus/value: focus=%d value=%q", m.editor.focus, m.editor.value(fieldMaxConn))
	}
	if m.editor.fields[m.editor.fieldIndex(fieldName)].label != "名称" || m.editor.errors[fieldMaxConn] != "必须是整数" {
		t.Fatalf("editor labels/errors were not localized: label=%q errors=%+v", m.editor.fields[m.editor.fieldIndex(fieldName)].label, m.editor.errors)
	}
}

func TestEditorTextInputKeepsLanguageShortcutRune(t *testing.T) {
	m := managedTestModel()
	m.view = viewEditor
	m.editor = newRuleEditorWithTranslator(editorEdit, testRule("running", true, 2201), m.config.Rules, m.tr)
	if !m.editor.focusField(fieldName) {
		t.Fatal("could not focus rule name")
	}
	before := m.editor.value(fieldName)
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	if m.overlay != overlayNone || m.editor.value(fieldName) != before+"l" {
		t.Fatalf("editor intercepted l as a shortcut: overlay=%v value=%q", m.overlay, m.editor.value(fieldName))
	}
}

func TestLanguageSwitchPreservesBotEditorState(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotEditor
	m.botConfig = &BotConfigResponse{BotToken: "123:token", BotChat: 42, BotControlToken: "control", ETag: `"bot-1"`}
	m.botEditor = newBotConfigEditorWithTranslator(m.botConfig, m.tr)
	m.botEditor.token.SetValue("123:changed")
	m.botEditor.token.Blur()
	m.botEditor.chat.SetValue("invalid")
	m.botEditor.chat.Focus()
	m.botEditor.focus = 1
	if _, err := m.botEditor.request(); err == nil || m.botEditor.chatError != "must be an integer" {
		t.Fatalf("English bot validation fixture failed: error=%v chat=%q", err, m.botEditor.chatError)
	}
	m.botEditor.formError = m.t("tui.status.load_latest_before_save")
	editor := m.botEditor

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyF1})
	m = updated.(Model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = updated.(Model)

	if m.botEditor != editor || m.botEditor.focus != 1 || !m.botEditor.chat.Focused() || !m.botEditor.dirty() {
		t.Fatalf("language switch rebuilt or reset bot editor: %+v", m.botEditor)
	}
	if m.botEditor.token.Value() != "123:changed" || m.botEditor.chat.Value() != "invalid" {
		t.Fatalf("language switch lost bot input: token=%q chat=%q", m.botEditor.token.Value(), m.botEditor.chat.Value())
	}
	if m.botEditor.token.Placeholder != "Telegram Bot 令牌" || m.botEditor.chatError != "必须是整数" {
		t.Fatalf("bot placeholders/errors were not localized: placeholder=%q error=%q", m.botEditor.token.Placeholder, m.botEditor.chatError)
	}
	if m.botEditor.formError != "保存前请按 Ctrl+R 加载最新远程配置。" {
		t.Fatalf("bot form error was not localized: %q", m.botEditor.formError)
	}
}

func TestApplyCannotBeSubmittedTwiceWhileOperationIsRunning(t *testing.T) {
	m := managedTestModel()
	m.view = viewRules
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	m.precheckResult = &PrecheckResponse{Precheck: precheck.Result{OK: true}}
	m.operation = operationApplying

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	m = updated.(Model)
	if cmd != nil || m.overlay == overlayApply || !strings.Contains(m.statusText, "wait") {
		t.Fatalf("second apply was not blocked: overlay=%v status=%q", m.overlay, m.statusText)
	}
	m.overlay = overlayApply
	updated, cmd = m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.operation != operationApplying || m.overlay != overlayNone {
		t.Fatalf("apply confirmation bypassed operation guard: operation=%v overlay=%v", m.operation, m.overlay)
	}
}

func TestApplyValidationFailureReturnsToFindings(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	payload, err := json.Marshal(PrecheckResponse{
		Revision: m.draft.BaseRevision,
		Precheck: precheck.Result{
			OK: false, ErrorCount: 1, CheckedRules: 2,
			Items: []precheck.Item{{Severity: precheck.SeverityError, Check: "listen_bind", RuleID: "running", Message: "busy"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.handleApplyResult(applyMsg{err: &APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Message:    "precheck failed",
		Body:       payload,
	}})
	m = updated.(Model)
	if m.view != viewPrecheck || m.sync != syncDirty || m.draft == nil || m.precheckResult == nil {
		t.Fatalf("validation apply state: view=%v sync=%v draft=%+v result=%+v", m.view, m.sync, m.draft, m.precheckResult)
	}
}

func TestLatePrecheckCannotClearStaleState(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.operation = operationPrechecking
	m.acceptConfig(&ConfigRulesResponse{
		Revision: "external", ETag: `"external"`, Writable: true, UDPMaxSessions: 256,
		Rules: []RuleInfo{testRule("running", true, 2201)},
	})
	if m.sync != syncStale {
		t.Fatalf("setup state = %v", m.sync)
	}
	updated, _ := m.handlePrecheckResult(precheckMsg{resp: &PrecheckResponse{
		Revision: m.draft.BaseRevision,
		Precheck: precheck.Result{OK: true, CheckedRules: len(m.draft.Rules), Items: []precheck.Item{}},
	}})
	m = updated.(Model)
	if m.sync != syncStale || m.precheckResult != nil {
		t.Fatalf("late precheck cleared stale state: sync=%v result=%+v", m.sync, m.precheckResult)
	}
}

func TestApply422WithoutFindingsDoesNotOpenEmptyResult(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	m.sync = syncValidated
	updated, _ := m.handleApplyResult(applyMsg{err: &APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Message:    "invalid request",
		Body:       []byte(`{"error":"invalid request"}`),
	}})
	m = updated.(Model)
	if m.view == viewPrecheck || m.precheckResult != nil || m.draft == nil {
		t.Fatalf("empty 422 created findings page: view=%v result=%+v draft=%+v", m.view, m.precheckResult, m.draft)
	}
}

func TestExternalRevisionMarksStaleWithoutReplacingDraft(t *testing.T) {
	m := managedTestModel()
	m.selectedRuleID = "running"
	m.stageToggle()
	want := cloneRules(m.draft.Rules)
	m.acceptConfig(&ConfigRulesResponse{
		Revision: "external", ETag: `"external"`, Writable: true, UDPMaxSessions: 64,
		Rules: []RuleInfo{testRule("external", true, 2300)},
	})
	if m.sync != syncStale || !reflectRulesEqual(m.draft.Rules, want) {
		t.Fatalf("external refresh overwrote draft: state=%v draft=%+v", m.sync, m.draft)
	}
}

func TestEditorPinsRevisionWhileExternalConfigChanges(t *testing.T) {
	m := managedTestModel()
	m.view = viewRules
	m.selectedRuleID = "running"
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = updated.(Model)
	if m.editor == nil || m.draft == nil || !m.transientDraft || m.draft.BaseRevision != "rev-1" {
		t.Fatalf("editor did not pin baseline: editor=%+v draft=%+v", m.editor, m.draft)
	}
	external := testRule("running", true, 2201)
	external.TargetPort = 2222
	m.acceptConfig(&ConfigRulesResponse{
		Revision: "rev-2", ETag: `"rev-2"`, Writable: true, UDPMaxSessions: 256,
		Rules: []RuleInfo{external, testRule("disabled", false, 2202)},
	})
	if m.sync != syncStale || m.config.Revision != "rev-1" || m.draft.BaseRevision != "rev-1" {
		t.Fatalf("external update replaced editor baseline: state=%v config=%+v draft=%+v", m.sync, m.config, m.draft)
	}
	rule, valid := m.editor.rule()
	if !valid {
		t.Fatalf("editor became invalid: %+v", m.editor.errors)
	}
	m.stageEditorRule(rule)
	if m.sync != syncStale || m.draft == nil || m.draft.BaseRevision != "rev-1" {
		t.Fatalf("saving stale editor bypassed conflict: state=%v draft=%+v", m.sync, m.draft)
	}
}

func TestCancelUDPSettingsDoesNotLeaveEmptyDraft(t *testing.T) {
	m := managedTestModel()
	m.view = viewRules
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m = updated.(Model)
	if m.overlay != overlayUDPSettings || m.draft == nil || !m.transientDraft {
		t.Fatalf("UDP settings did not pin a transient draft: overlay=%v draft=%+v", m.overlay, m.draft)
	}
	updated, _ = m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.overlay != overlayNone || m.draft != nil || m.sync != syncClean {
		t.Fatalf("cancel left transient state: overlay=%v draft=%+v sync=%v", m.overlay, m.draft, m.sync)
	}
}

func TestRuleStateDetectsRuntimeDriftAndMissingRule(t *testing.T) {
	m := managedTestModel()
	running := *m.baselineRule("running")
	if got := m.ruleState(running); got != "RUNNING" {
		t.Fatalf("matching state = %q", got)
	}
	drifted := running
	drifted.TargetPort = 2222
	m.rules = &RulesResponse{Items: []RuleInfo{drifted}}
	if got := m.ruleState(running); got != "DRIFT" {
		t.Fatalf("drift state = %q", got)
	}
	m.rules = &RulesResponse{Items: []RuleInfo{}}
	if got := m.ruleState(running); got != "ERROR" {
		t.Fatalf("missing enabled state = %q", got)
	}
	disabled := *m.baselineRule("disabled")
	if got := m.ruleState(disabled); got != "DISABLED" {
		t.Fatalf("disabled state = %q", got)
	}
}

func TestEscReturnsFromDetail(t *testing.T) {
	m := managedTestModel()
	m.view = viewDetail
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(Model).view != viewRules {
		t.Fatalf("Esc view = %v", updated.(Model).view)
	}
}

func TestEditorValidationAndProtocolCycle(t *testing.T) {
	rule := testRule("one", true, 2201)
	editor := newRuleEditor(editorEdit, rule, []RuleInfo{rule})
	parsed, ok := editor.rule()
	if !ok || parsed.RuleID != rule.RuleID {
		t.Fatalf("valid edit failed: errors=%+v", editor.errors)
	}
	for editor.fields[editor.focus].key != fieldProtocol {
		editor.moveFocus(1)
	}
	editor.update(tea.KeyMsg{Type: tea.KeyRight})
	parsed, ok = editor.rule()
	if !ok || parsed.Protocol != engine.ProtocolUDP {
		t.Fatalf("protocol cycle = %s errors=%+v", parsed.Protocol, editor.errors)
	}

	create := newRuleEditor(editorCreate, RuleInfo{Protocol: engine.ProtocolTCP, Enabled: true}, nil)
	if _, ok := create.rule(); ok || create.errors[fieldRuleID] == "" || create.errors[fieldListenPort] == "" {
		t.Fatalf("invalid create errors = %+v", create.errors)
	}
}

func TestEditorSourceIPPolicyAndCopy(t *testing.T) {
	rule := testRule("protected", true, 2201)
	editor := newRuleEditor(editorEdit, rule, []RuleInfo{rule})
	for index := range editor.fields {
		switch editor.fields[index].key {
		case fieldSourceIPMode:
			editor.fields[index].choice = string(engine.SourceIPModeAllowlist)
		case fieldSourceIPs:
			editor.fields[index].input.SetValue("192.0.2.1, 2001:db8::/32")
		}
	}
	parsed, ok := editor.rule()
	if !ok || parsed.SourceIPMode != engine.SourceIPModeAllowlist || len(parsed.SourceIPs) != 2 {
		t.Fatalf("source policy edit = %+v errors=%+v", parsed, editor.errors)
	}

	copied := copyRule(parsed, []RuleInfo{parsed})
	copied.SourceIPs[0] = "198.51.100.1"
	if parsed.SourceIPs[0] != "192.0.2.1" {
		t.Fatalf("copied rule shared source IP storage: %+v", parsed.SourceIPs)
	}
}

func TestEditorTrafficControlsRoundTripAndRejectMixedBandwidthModes(t *testing.T) {
	rule := testRule("limited", true, 2201)
	rule.Bandwidth = &engine.BandwidthLimit{Rate: engine.ByteRate(12_500_000)}
	rule.TrafficQuota = &engine.TrafficQuota{
		Daily:    engine.ByteSize(100_000_000_000),
		Monthly:  engine.ByteSize(1 << 40),
		Timezone: "Asia/Shanghai",
		Action:   "block",
	}
	editor := newRuleEditor(editorEdit, rule, []RuleInfo{rule})
	if got := editor.value(fieldBandwidthRate); got != "100 Mbps" {
		t.Fatalf("bandwidth editor value = %q", got)
	}
	if got := editor.value(fieldQuotaDaily); got != "100 GB" {
		t.Fatalf("daily quota editor value = %q", got)
	}
	if got := editor.value(fieldQuotaMonthly); got != "1 TiB" {
		t.Fatalf("monthly quota editor value = %q", got)
	}
	parsed, valid := editor.rule()
	if !valid {
		t.Fatalf("traffic controls did not round-trip: %+v", editor.errors)
	}
	if parsed.Bandwidth == nil || parsed.Bandwidth.Rate != rule.Bandwidth.Rate || parsed.TrafficQuota == nil || *parsed.TrafficQuota != *rule.TrafficQuota {
		t.Fatalf("traffic controls changed during round-trip: %+v", parsed)
	}

	editor.fields[editor.fieldIndex(fieldBandwidthUpload)].input.SetValue("50 Mbps")
	if _, valid := editor.rule(); valid || editor.errors[fieldBandwidthRate] == "" || editor.errors[fieldBandwidthUpload] == "" {
		t.Fatalf("mixed bandwidth modes were accepted: %+v", editor.errors)
	}
	editor.fields[editor.fieldIndex(fieldBandwidthUpload)].input.SetValue("")
	editor.fields[editor.fieldIndex(fieldQuotaTimezone)].input.SetValue("Not/AZone")
	if _, valid := editor.rule(); valid || editor.errors[fieldQuotaTimezone] == "" {
		t.Fatalf("invalid quota timezone was accepted: %+v", editor.errors)
	}

	copied := copyRule(rule, []RuleInfo{rule})
	copied.Bandwidth.Rate = 1
	copied.TrafficQuota.Daily = 1
	if rule.Bandwidth.Rate != engine.ByteRate(12_500_000) || rule.TrafficQuota.Daily != engine.ByteSize(100_000_000_000) {
		t.Fatal("copied rule shares bandwidth or quota pointers with its source")
	}
}

func TestViewSmokeAtCommonSizes(t *testing.T) {
	for _, size := range []struct{ width, height int }{{60, 20}, {100, 30}, {160, 45}} {
		m := managedTestModel()
		m.width, m.height = size.width, size.height
		m.view = viewRules
		if output := m.View(); output == "" || !strings.Contains(output, "RULES") {
			t.Fatalf("rules view %dx%d = %q", size.width, size.height, output)
		}
		m.editor = newRuleEditor(editorEdit, testRule("running", true, 2201), m.config.Rules)
		m.view = viewEditor
		if output := m.View(); !strings.Contains(output, "Edit Rule") {
			t.Fatalf("editor view %dx%d omitted title", size.width, size.height)
		}
	}
}

func TestStaleBotRefreshCannotCloseEditorOrOverwriteSave(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotEditor
	m.botConfig = &BotConfigResponse{
		Revision: "bot-rev-1", ETag: `"bot-rev-1"`,
		BotToken: "123:old", BotChat: 42, BotControlToken: "admin-old",
	}
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.botEditor.token.SetValue("123:new")
	m.botRequestID = 2
	m.botOperation = botOperationSaving

	updated, _ := m.Update(botConfigMsg{
		requestID: 1,
		operation: botOperationFetching,
		resp: &BotConfigResponse{
			Revision: "bot-rev-1", ETag: `"bot-rev-1"`,
			BotToken: "123:old", BotChat: 42,
		},
	})
	m = updated.(Model)
	if m.view != viewBotEditor || m.botEditor == nil || m.botEditor.token.Value() != "123:new" {
		t.Fatalf("stale refresh changed active editor: view=%v editor=%+v", m.view, m.botEditor)
	}
	if m.botConfig.BotToken != "123:old" || m.botOperation != botOperationSaving {
		t.Fatalf("stale refresh changed state: config=%+v operation=%v", m.botConfig, m.botOperation)
	}

	updated, _ = m.Update(botConfigMsg{
		requestID: 2,
		operation: botOperationSaving,
		resp: &BotConfigResponse{
			Revision: "bot-rev-2", ETag: `"bot-rev-2"`,
			BotToken: "123:new", BotChat: 42, Running: true,
		},
	})
	m = updated.(Model)
	if m.view != viewBotConfig || m.botEditor != nil || m.botOperation != botOperationIdle {
		t.Fatalf("save result did not finish editor: view=%v editor=%+v operation=%v", m.view, m.botEditor, m.botOperation)
	}
	if m.botConfig == nil || m.botConfig.BotToken != "123:new" {
		t.Fatalf("save response not retained: %+v", m.botConfig)
	}
}

func TestBotOperationsBlockDuplicateSubmission(t *testing.T) {
	m := managedTestModel()
	m.botConfig = &BotConfigResponse{
		Revision: "bot-rev-1", ETag: `"bot-rev-1"`,
		BotToken: "123:token", BotChat: 42,
	}
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.view = viewBotEditor

	updated, first := m.handleBotEditorKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	requestID := m.botRequestID
	if first == nil || m.botOperation != botOperationSaving {
		t.Fatalf("save did not enter pending state: operation=%v cmd=%v", m.botOperation, first)
	}
	updated, second := m.handleBotEditorKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if second != nil || m.botRequestID != requestID || m.botOperation != botOperationSaving {
		t.Fatalf("duplicate save was not blocked: id=%d operation=%v cmd=%v", m.botRequestID, m.botOperation, second)
	}

	m.botOperation = botOperationIdle
	m.view = viewBotConfig
	updated, first = m.handleBotConfigKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	requestID = m.botRequestID
	if first == nil || m.botOperation != botOperationStarting {
		t.Fatalf("start did not enter pending state: operation=%v cmd=%v", m.botOperation, first)
	}
	updated, second = m.handleBotConfigKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	if second != nil || m.botRequestID != requestID || m.botOperation != botOperationStarting {
		t.Fatalf("duplicate start was not blocked: id=%d operation=%v cmd=%v", m.botRequestID, m.botOperation, second)
	}
}

func TestBotValidationFailureKeepsEditor(t *testing.T) {
	m := managedTestModel()
	m.botConfig = &BotConfigResponse{
		Revision: "bot-rev-1", ETag: `"bot-rev-1"`,
		BotToken: "123:old", BotChat: 42,
	}
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.botEditor.token.SetValue("invalid")
	m.view = viewBotEditor
	m.botRequestID = 7
	m.botOperation = botOperationSaving

	updated, _ := m.Update(botConfigMsg{
		requestID: 7,
		operation: botOperationSaving,
		err:       &APIError{StatusCode: http.StatusUnprocessableEntity, Message: "invalid bot configuration"},
	})
	m = updated.(Model)
	if m.view != viewBotEditor || m.botEditor == nil || m.botEditor.token.Value() != "invalid" {
		t.Fatalf("validation error discarded editor: view=%v editor=%+v", m.view, m.botEditor)
	}
	if m.botOperation != botOperationIdle || !strings.Contains(m.statusText, "invalid Telegram token") {
		t.Fatalf("validation status = %q operation=%v", m.statusText, m.botOperation)
	}
}

func TestBotPanelQuitProtectsRulesDraft(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotConfig
	m.draft = &draftConfig{BaseRevision: "rev-1", BaseETag: `"rev-1"`}

	updated, cmd := m.handleBotConfigKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = updated.(Model)
	if cmd != nil || m.overlay != overlayQuitDirty || m.draft == nil {
		t.Fatalf("quit bypassed draft protection: overlay=%v draft=%+v cmd=%v", m.overlay, m.draft, cmd)
	}
}

func TestBotPanelBlocksEditingWhileRulesDraftExists(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotConfig
	m.botConfig = &BotConfigResponse{Revision: "rev-1", ETag: `"rev-1"`, BotToken: "123:token", BotChat: 42}
	m.draft = &draftConfig{BaseRevision: "rev-1", BaseETag: `"rev-1"`}

	updated, cmd := m.handleBotConfigKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = updated.(Model)
	if cmd != nil || m.view != viewBotConfig || m.botEditor != nil || !strings.Contains(m.statusText, "rules draft") {
		t.Fatalf("bot edit bypassed rules draft guard: view=%v editor=%+v status=%q cmd=%v", m.view, m.botEditor, m.statusText, cmd)
	}
}

func TestBotSaveRefreshesSharedConfigRevision(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotEditor
	m.botConfig = &BotConfigResponse{Revision: "rev-1", ETag: `"rev-1"`, BotToken: "123:old", BotChat: 42}
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.botRequestID = 3
	m.botOperation = botOperationSaving
	previousConfigRequestID := m.configRequestID

	updated, cmd := m.Update(botConfigMsg{
		requestID: 3,
		operation: botOperationSaving,
		resp:      &BotConfigResponse{Revision: "rev-2", ETag: `"rev-2"`, BotToken: "123:new", BotChat: 42, Running: true},
	})
	m = updated.(Model)
	if cmd == nil || m.configRequestID != previousConfigRequestID+1 || m.lastConfigResponseID != m.configRequestID {
		t.Fatalf("bot save did not refresh shared config: request=%d last=%d cmd=%v", m.configRequestID, m.lastConfigResponseID, cmd)
	}
	if m.configBarrierID != m.configRequestID {
		t.Fatalf("config barrier = %d, request = %d", m.configBarrierID, m.configRequestID)
	}
	if m.view != viewBotConfig || m.botEditor != nil || m.botConfig == nil || m.botConfig.Revision != "rev-2" {
		t.Fatalf("bot save state = view:%v editor:%+v config:%+v", m.view, m.botEditor, m.botConfig)
	}

	m.view = viewRules
	updated, blocked := m.handleManagementKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)
	if blocked != nil || m.editor != nil || m.draft != nil || !strings.Contains(m.statusText, "configuration refresh") {
		t.Fatalf("rule edit crossed config barrier: editor=%+v draft=%+v status=%q cmd=%v", m.editor, m.draft, m.statusText, blocked)
	}

	updated, _ = m.Update(configRulesMsg{
		requestID: m.configRequestID,
		resp: &ConfigRulesResponse{
			Revision: "rev-2", ETag: `"rev-2"`, Writable: true, UDPMaxSessions: 256,
			Rules: cloneRules(m.config.Rules),
		},
	})
	m = updated.(Model)
	if m.configBarrierID != 0 || m.config == nil || m.config.Revision != "rev-2" {
		t.Fatalf("config barrier did not clear: barrier=%d config=%+v", m.configBarrierID, m.config)
	}
	updated, _ = m.handleManagementKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)
	if m.editor == nil || m.draft == nil {
		t.Fatalf("rule edit remained blocked after config refresh: editor=%+v draft=%+v", m.editor, m.draft)
	}
}

func TestBotConfig503DistinguishesReadinessCommitAndRollback(t *testing.T) {
	tests := []struct {
		name           string
		message        string
		body           string
		wantRefresh    bool
		wantEditor     bool
		wantStatusText string
	}{
		{
			name:           "readiness",
			message:        "bot readiness could not be established",
			body:           `{"error":"bot readiness could not be established","running":true}`,
			wantEditor:     true,
			wantStatusText: "previous bot is unchanged",
		},
		{
			name:           "committed durability failure",
			message:        "configuration committed but durability could not be confirmed",
			body:           `{"error":"configuration committed but durability could not be confirmed","committed":true,"revision":"rev-2","running":true}`,
			wantRefresh:    true,
			wantStatusText: "committed, but durability",
		},
		{
			name:           "rollback failure",
			message:        "configuration could not be committed",
			body:           `{"error":"configuration could not be committed","rollback_ok":false,"running":true}`,
			wantRefresh:    true,
			wantEditor:     true,
			wantStatusText: "rollback is uncertain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := managedTestModel()
			m.view = viewBotEditor
			m.botConfig = &BotConfigResponse{Revision: "rev-1", ETag: `"rev-1"`, BotToken: "123:old", BotChat: 42}
			m.botEditor = newBotConfigEditor(m.botConfig)
			m.botRequestID = 8
			m.botOperation = botOperationSaving

			updated, cmd := m.Update(botConfigMsg{
				requestID: 8,
				operation: botOperationSaving,
				err:       &APIError{StatusCode: http.StatusServiceUnavailable, Message: tt.message, Body: []byte(tt.body)},
			})
			m = updated.(Model)
			if (cmd != nil) != tt.wantRefresh {
				t.Fatalf("refresh cmd = %v, want refresh %v", cmd, tt.wantRefresh)
			}
			if tt.wantRefresh && m.botOperation != botOperationFetching {
				t.Fatalf("bot operation = %v, want fetching", m.botOperation)
			}
			if !tt.wantRefresh && m.botOperation != botOperationIdle {
				t.Fatalf("bot operation = %v, want idle", m.botOperation)
			}
			if (m.botEditor != nil) != tt.wantEditor {
				t.Fatalf("editor retained = %v, want %v", m.botEditor != nil, tt.wantEditor)
			}
			if !strings.Contains(m.statusText, tt.wantStatusText) {
				t.Fatalf("status = %q, want %q", m.statusText, tt.wantStatusText)
			}
		})
	}
}

func TestBotConfigFetch503DoesNotRetryForever(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotConfig
	m.botRequestID = 4
	m.botOperation = botOperationFetching

	updated, cmd := m.Update(botConfigMsg{
		requestID: 4,
		operation: botOperationFetching,
		err:       &APIError{StatusCode: http.StatusServiceUnavailable, Message: "upstream unavailable"},
	})
	m = updated.(Model)
	if cmd != nil || m.botOperation != botOperationIdle || !strings.Contains(m.statusText, "unavailable") {
		t.Fatalf("fetch 503 retried unexpectedly: operation=%v status=%q cmd=%v", m.botOperation, m.statusText, cmd)
	}
}

func TestDashboardBotShortcutAndResponsiveLayout(t *testing.T) {
	m := managedTestModel()
	m.width = 80
	m.height = 24
	m.view = viewDashboard
	output := m.View()
	if lines := strings.Count(output, "\n") + 1; lines > m.height {
		t.Fatalf("dashboard height = %d, terminal height = %d\n%s", lines, m.height, output)
	}

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = updated.(Model)
	if cmd == nil || m.view != viewBotConfig || m.botOperation != botOperationFetching {
		t.Fatalf("dashboard bot shortcut failed: view=%v operation=%v cmd=%v", m.view, m.botOperation, cmd)
	}

	m = managedTestModel()
	m.width = 60
	m.height = 20
	m.view = viewRules
	output = m.View()
	for _, hint := range []string{"[n]ew", "[enter]detail", "[1]dashboard", "[?]help"} {
		if !strings.Contains(output, hint) {
			t.Fatalf("60-column rules view omitted %q:\n%s", hint, output)
		}
	}
	if strings.Contains(output, "[P]recheck") || strings.Contains(output, "[A]pply") {
		t.Fatalf("rules view advertised unavailable draft actions:\n%s", output)
	}
	if lines := strings.Count(output, "\n") + 1; lines > m.height {
		t.Fatalf("rules height = %d, terminal height = %d\n%s", lines, m.height, output)
	}

	m.width = 80
	m.height = 24
	m.overlay = overlayHelp
	output = m.View()
	if lines := strings.Count(output, "\n") + 1; lines > m.height {
		t.Fatalf("help overlay height = %d, terminal height = %d\n%s", lines, m.height, output)
	}
}

func TestNumberNavigationAndDraftPrimaryAction(t *testing.T) {
	m := managedTestModel()
	m.view = viewDashboard

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = updated.(Model)
	if cmd != nil || m.view != viewRules {
		t.Fatalf("number navigation failed: view=%v cmd=%v", m.view, cmd)
	}

	m.selectedRuleID = "running"
	m.stageToggle()
	updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.operation != operationPrechecking {
		t.Fatalf("Enter did not start the draft precheck: operation=%v cmd=%v", m.operation, cmd)
	}

	m.operation = operationIdle
	updated, _ = m.handlePrecheckResult(precheckMsg{resp: &PrecheckResponse{
		Revision: m.draft.BaseRevision,
		Precheck: precheck.Result{OK: true, CheckedRules: len(m.draft.Rules), Items: []precheck.Item{}},
	}})
	m = updated.(Model)
	updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.overlay != overlayApply {
		t.Fatalf("Enter did not advance a passed precheck to apply confirmation: overlay=%v cmd=%v", m.overlay, cmd)
	}
}

func TestCreateRuleUsesBasicModeAndAutomaticID(t *testing.T) {
	m := managedTestModel()
	m.view = viewRules
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)
	if m.editor == nil || m.editor.advanced || m.editor.fields[m.editor.focus].key != fieldName {
		t.Fatalf("new rule editor did not start in the name-first basic flow: %+v", m.editor)
	}
	if got := m.editor.value(fieldRuleID); got != "new-rule" {
		t.Fatalf("default rule ID = %q, want new-rule", got)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Tokyo Edge")})
	m = updated.(Model)
	if got := m.editor.value(fieldRuleID); got != "tokyo-edge" {
		t.Fatalf("automatic rule ID = %q, want tokyo-edge", got)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyF2})
	m = updated.(Model)
	if !m.editor.advanced {
		t.Fatal("F2 did not reveal advanced settings")
	}
}

func managedTestModel() Model {
	return managedTestModelWithTranslator(i18n.New(i18n.English))
}

func managedTestModelWithTranslator(translator *i18n.Translator) Model {
	m := newModelWithOptions(context.Background(), "http://127.0.0.1:19090", RunOptions{Translator: translator})
	m.ready = true
	m.width = 120
	m.height = 32
	m.session = &SessionResponse{Actor: "admin", Role: "admin", Capabilities: SessionCapabilities{RulesWrite: true}}
	m.config = &ConfigRulesResponse{
		Revision: "rev-1", ETag: `"rev-1"`, Writable: true, UDPMaxSessions: 256,
		Rules: []RuleInfo{testRule("running", true, 2201), testRule("disabled", false, 2202)},
	}
	m.rules = &RulesResponse{Items: []RuleInfo{testRule("running", true, 2201)}}
	m.sync = syncClean
	m.reconcileSelection()
	return m
}

func testRule(id string, enabled bool, port int) RuleInfo {
	return RuleInfo{
		RuleID: id, Name: id, Protocol: engine.ProtocolTCP,
		ListenAddr: "127.0.0.1", ListenPort: port,
		TargetAddr: "127.0.0.1", TargetPort: 22,
		Enabled: enabled, IdleTimeout: 30,
		Revision: 9, CreatedTime: 10, UpdatedTime: 11,
	}
}

func reflectRulesEqual(left, right []RuleInfo) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].RuleID != right[index].RuleID || left[index].Enabled != right[index].Enabled || left[index].Revision != right[index].Revision {
			return false
		}
	}
	return true
}
