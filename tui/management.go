package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudapp3/vmflow/engine"
)

type localRuleDiff struct {
	Added    []string
	Updated  []string
	Enabled  []string
	Disabled []string
	Deleted  []string
}

func (diff localRuleDiff) changeCount() int {
	return len(diff.Added) + len(diff.Updated) + len(diff.Enabled) + len(diff.Disabled) + len(diff.Deleted)
}

func (m *Model) acceptConfig(response *ConfigRulesResponse) {
	if response == nil {
		return
	}
	next := *response
	next.Rules = cloneRules(response.Rules)
	if m.draft != nil {
		if next.Revision != m.draft.BaseRevision {
			m.remoteRevision = next.Revision
			m.sync = syncStale
			m.setStatus("server configuration changed; local draft retained")
			return
		}
		m.config = &next
		if m.sync == syncStale {
			m.remoteRevision = ""
			m.precheckResult = nil
			m.sync = syncDirty
			m.setStatus("configuration reconciled; precheck the retained draft again")
		}
		return
	}
	m.config = &next
	m.remoteRevision = ""
	m.sync = syncClean
	m.reconcileSelection()
}

func (m Model) displayRules() []RuleInfo {
	if m.draft != nil {
		return m.draftDisplayRules()
	}
	if m.config != nil {
		return cloneRules(m.config.Rules)
	}
	if m.rules != nil {
		return cloneRules(m.rules.Items)
	}
	return nil
}

func (m Model) draftDisplayRules() []RuleInfo {
	if m.draft == nil {
		return nil
	}
	current := make(map[string]RuleInfo, len(m.draft.Rules))
	for _, rule := range m.draft.Rules {
		current[rule.RuleID] = rule
	}
	result := make([]RuleInfo, 0, len(m.draft.Rules)+len(m.draft.Deleted))
	seen := make(map[string]struct{}, cap(result))
	if m.config != nil {
		for _, baseline := range m.config.Rules {
			if rule, ok := current[baseline.RuleID]; ok {
				result = append(result, rule)
				seen[rule.RuleID] = struct{}{}
				continue
			}
			if rule, ok := m.draft.Deleted[baseline.RuleID]; ok {
				result = append(result, rule)
				seen[rule.RuleID] = struct{}{}
			}
		}
	}
	for _, rule := range m.draft.Rules {
		if _, ok := seen[rule.RuleID]; !ok {
			result = append(result, rule)
		}
	}
	return cloneRules(result)
}

func (m Model) canWrite() bool {
	return m.session != nil && m.session.Capabilities.RulesWrite && m.config != nil && m.config.Writable
}

func (m Model) canReload() bool {
	return m.session != nil && strings.EqualFold(strings.TrimSpace(m.session.Role), "admin")
}

func (m Model) writeDeniedReason() string {
	switch {
	case m.session == nil:
		if m.sessionErr != nil {
			return "management unavailable: " + m.sessionErr.Error()
		}
		return "management unavailable: session not loaded"
	case !m.session.Capabilities.RulesWrite:
		if strings.EqualFold(strings.TrimSpace(m.session.Role), "admin") {
			return "rule writes require configured authenticated admin access"
		}
		return "read-only session: admin role required"
	case m.config == nil:
		return "management unavailable: configuration not loaded"
	case !m.config.Writable:
		return "configuration file is read-only"
	default:
		return "management unavailable"
	}
}

func (m *Model) ensureDraft() bool {
	if m.draft != nil {
		return true
	}
	if !m.canWrite() {
		m.setStatus(m.writeDeniedReason())
		return false
	}
	m.draft = &draftConfig{
		BaseRevision:   m.config.Revision,
		BaseETag:       m.config.ETag,
		UDPMaxSessions: m.config.UDPMaxSessions,
		Rules:          cloneRules(m.config.Rules),
		Deleted:        make(map[string]RuleInfo),
	}
	return true
}

func (m *Model) refreshDraftState() {
	m.precheckResult = nil
	m.applyResult = nil
	if m.draft == nil {
		m.sync = syncClean
		return
	}
	if m.sync != syncStale && m.config != nil && m.draft.UDPMaxSessions == m.config.UDPMaxSessions && reflect.DeepEqual(m.draft.Rules, m.config.Rules) {
		m.draft = nil
		m.sync = syncClean
		m.remoteRevision = ""
		m.reconcileSelection()
		return
	}
	if m.sync != syncStale {
		m.sync = syncDirty
	}
	m.reconcileSelection()
}

func (m Model) handleManagementKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "n":
		if !m.prepareWrite() {
			return m, nil
		}
		m.beginTransientDraft()
		defaults := RuleInfo{Protocol: engine.ProtocolTCP, ListenAddr: "0.0.0.0", Enabled: true}
		m.editor = newRuleEditor(editorCreate, defaults, m.editableRules())
		m.view = viewEditor
	case "e":
		if !m.prepareWrite() {
			return m, nil
		}
		rule := m.selectedRule()
		if rule == nil {
			m.setStatus("select a rule first")
			return m, nil
		}
		if m.isDeleted(rule.RuleID) {
			m.setStatus("rule is staged for deletion")
			return m, nil
		}
		m.beginTransientDraft()
		m.editor = newRuleEditor(editorEdit, *rule, m.editableRules())
		m.view = viewEditor
	case "c":
		if !m.prepareWrite() {
			return m, nil
		}
		rule := m.selectedRule()
		if rule == nil {
			m.setStatus("select a rule first")
			return m, nil
		}
		if m.isDeleted(rule.RuleID) {
			m.setStatus("rule is staged for deletion")
			return m, nil
		}
		m.beginTransientDraft()
		copy := copyRule(*rule, m.editableRules())
		m.editor = newRuleEditor(editorCopy, copy, m.editableRules())
		m.view = viewEditor
	case " ":
		if !m.prepareWrite() {
			return m, nil
		}
		m.stageToggle()
	case "d":
		if !m.prepareWrite() {
			return m, nil
		}
		rule := m.selectedRule()
		if rule == nil {
			m.setStatus("select a rule first")
		} else if m.isDeleted(rule.RuleID) {
			m.setStatus("rule is already staged for deletion")
		} else {
			m.overlay = overlayDelete
		}
	case "P":
		return m.startPrecheck()
	case "A":
		if m.operation != operationIdle {
			m.setStatus("wait for the current operation to finish")
		} else if m.configBarrierID != 0 {
			m.setStatus("wait for the shared configuration refresh to finish")
		} else if m.sync == syncStale {
			m.setStatus("server configuration changed; discard or refresh before applying")
		} else if m.sync != syncValidated || m.precheckResult == nil || !m.precheckResult.Precheck.OK {
			m.setStatus("precheck the current draft first (P)")
		} else {
			m.overlay = overlayApply
		}
	case "u":
		if m.operation != operationIdle {
			m.setStatus("wait for the current operation to finish")
		} else if m.draft == nil {
			m.setStatus("no local draft")
		} else {
			m.overlay = overlayDiscardDraft
		}
	case "g":
		if !m.prepareWrite() {
			return m, nil
		}
		m.beginTransientDraft()
		m.openUDPSettings()
	}
	return m, nil
}

// handlePendingOperationKey prevents navigation or edits from hiding an
// in-flight result or discarding the draft used by that operation. It should
// run at the start of the global key handler, before overlay/view routing.
func (m Model) handlePendingOperationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.operation == operationIdle {
		return m, nil, false
	}
	switch msg.String() {
	case "q", "ctrl+c", "esc", "tab", "enter", "y", "n", "e", "c", " ", "d", "P", "A", "u", "g", "R", "b", "s", "x", "ctrl+s", "?", "f1":
		operation := "operation"
		switch m.operation {
		case operationPrechecking:
			operation = "precheck"
		case operationApplying:
			operation = "apply"
		case operationReloading:
			operation = "reload"
		}
		m.setStatus(operation + " in progress; wait for the result before leaving or changing configuration")
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m Model) openBotConfig() (tea.Model, tea.Cmd) {
	if m.operation != operationIdle {
		m.setStatus("wait for the current operation to finish")
		return m, nil
	}
	if !m.canWrite() {
		m.setStatus(m.writeDeniedReason())
		return m, nil
	}
	if m.botOperation != botOperationIdle {
		m.setStatus("wait for the current bot operation to finish")
		return m, nil
	}
	m.view = viewBotConfig
	requestID := m.beginBotOperation(botOperationFetching)
	return m, fetchBotConfigCmd(m.ctx, m.client, requestID)
}

func (m Model) handleBotConfigKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		if m.draft != nil {
			m.overlay = overlayQuitDirty
			return m, nil
		}
		return m, tea.Quit
	case "?", "f1":
		m.overlayYOffset = 0
		m.overlay = overlayHelp
	case "esc":
		m.view = viewRules
	case "r":
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
			return m, nil
		}
		requestID := m.beginBotOperation(botOperationFetching)
		return m, fetchBotConfigCmd(m.ctx, m.client, requestID)
	case "e":
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
			return m, nil
		}
		if m.botConfigErr != nil {
			m.setStatus("refresh bot state before editing configuration")
			return m, nil
		}
		if m.draft != nil {
			m.setStatus("apply or discard the rules draft before editing bot config")
			return m, nil
		}
		if m.botConfig == nil {
			m.setStatus("bot config not loaded yet")
			return m, nil
		}
		m.botEditor = newBotConfigEditor(m.botConfig)
		m.view = viewBotEditor
	case "s":
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
			return m, nil
		}
		if m.botConfigErr != nil {
			m.setStatus("refresh bot state before starting the bot")
			return m, nil
		}
		if m.botConfig != nil && m.botConfig.Running {
			m.setStatus("bot is already running")
			return m, nil
		}
		if !botReadyToStart(m.botConfig) {
			m.setStatus("configure a bot token and chat ID before starting")
			return m, nil
		}
		requestID := m.beginBotOperation(botOperationStarting)
		return m, startBotCmd(m.ctx, m.client, requestID)
	case "x":
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
		} else if m.botConfigErr != nil {
			m.setStatus("refresh bot state before stopping the bot")
		} else if m.botConfig != nil && m.botConfig.Running {
			m.overlay = overlayBotStop
		} else {
			m.setStatus("bot is not running")
		}
	}
	return m, nil
}

func (m Model) handleBotEditorKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.botEditor == nil {
		m.view = viewBotConfig
		return m, nil
	}
	switch msg.String() {
	case "f1":
		m.overlayYOffset = 0
		m.overlay = overlayHelp
		return m, nil
	case "ctrl+r":
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
			return m, nil
		}
		m.botRebasePending = true
		m.botEditor.formError = "Loading the latest remote configuration..."
		m.setStatus("refreshing bot config for rebase")
		requestID := m.beginBotOperation(botOperationFetching)
		return m, fetchBotConfigCmd(m.ctx, m.client, requestID)
	case "esc", "ctrl+c", "ctrl+s":
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
			return m, nil
		}
		if msg.String() == "esc" || msg.String() == "ctrl+c" {
			if m.botEditor.dirty() {
				m.overlay = overlayCancelBotEditor
				return m, nil
			}
			m.botEditor = nil
			m.botRebasePending = false
			m.view = viewBotConfig
			return m, nil
		}
		if m.botRebasePending || m.botConfigErr != nil {
			m.botEditor.formError = "Load the latest remote configuration with Ctrl+R before saving."
			m.setStatus("rebase the latest bot config before saving")
			return m, nil
		}
		req, err := m.botEditor.request()
		if err != nil {
			m.setStatus(err.Error())
			return m, nil
		}
		etag := m.botEditor.etag
		m.setStatus("applying bot config...")
		requestID := m.beginBotOperation(botOperationSaving)
		return m, applyBotConfigCmd(m.ctx, m.client, requestID, etag, req)
	default:
		if m.botOperation != botOperationIdle {
			m.setStatus("wait for the current bot operation to finish")
			return m, nil
		}
		cmd := m.botEditor.update(msg)
		if m.botRebasePending {
			m.botEditor.formError = "Latest remote configuration is required; press Ctrl+R to retry."
		}
		return m, cmd
	}
}

func (m *Model) beginBotOperation(operation botOperationState) int64 {
	m.botRequestID++
	m.botOperation = operation
	return m.botRequestID
}

func botReadyToStart(cfg *BotConfigResponse) bool {
	return cfg != nil && strings.TrimSpace(cfg.BotToken) != "" && cfg.BotChat != 0
}

func (m *Model) beginConfigBarrier() int64 {
	m.configRequestID++
	m.lastConfigResponseID = m.configRequestID
	m.configBarrierID = m.configRequestID
	return m.configRequestID
}

func startBotCmd(ctx context.Context, client *Client, requestID int64) tea.Cmd {
	return func() tea.Msg {
		return botActionMsg{
			requestID: requestID,
			operation: botOperationStarting,
			action:    "start",
			err:       client.StartBot(ctx),
		}
	}
}

func stopBotCmd(ctx context.Context, client *Client, requestID int64) tea.Cmd {
	return func() tea.Msg {
		return botActionMsg{
			requestID: requestID,
			operation: botOperationStopping,
			action:    "stop",
			err:       client.StopBot(ctx),
		}
	}
}

func applyBotConfigCmd(ctx context.Context, client *Client, requestID int64, etag string, req BotConfigRequest) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.ApplyBotConfig(ctx, etag, req)
		return botConfigMsg{
			requestID: requestID,
			operation: botOperationSaving,
			resp:      resp,
			err:       err,
		}
	}
}

func botConfigUnavailableState(err error) (message string, refresh, committed bool) {
	apiErr, ok := err.(*APIError)
	if !ok {
		return "bot config failed; current state is uncertain; refreshing", true, false
	}
	var payload struct {
		Committed  bool  `json:"committed"`
		RollbackOK *bool `json:"rollback_ok"`
	}
	_ = json.Unmarshal(apiErr.Body, &payload)
	if payload.Committed {
		return "bot config committed, but durability could not be confirmed; refreshing", true, true
	}
	if payload.RollbackOK != nil && !*payload.RollbackOK {
		return "bot config commit failed and runtime rollback is uncertain; refreshing", true, false
	}
	if strings.Contains(strings.ToLower(apiErr.Message), "readiness") {
		return "bot readiness failed; the previous bot is unchanged", false, false
	}
	return "bot config failed; current state is uncertain; refreshing", true, false
}

func rulesApplyUnavailableState(err error) (message string, reconcile, committed, handled bool) {
	apiErr, ok := err.(*APIError)
	if !ok {
		return "apply result is uncertain; draft retained; reconciling config and runtime", true, false, true
	}
	var payload struct {
		Committed  bool  `json:"committed"`
		RollbackOK *bool `json:"rollback_ok"`
	}
	_ = json.Unmarshal(apiErr.Body, &payload)
	if payload.Committed {
		return "apply committed, but durability could not be confirmed; reconciling config and runtime", true, true, true
	}
	if payload.RollbackOK != nil {
		if *payload.RollbackOK {
			return "apply failed; runtime rolled back; draft retained", false, false, true
		}
		return "apply failed and runtime rollback is uncertain; draft retained; reconciling config and runtime", true, false, true
	}
	if apiErr.StatusCode == http.StatusServiceUnavailable {
		return "apply failed and current state is uncertain; draft retained; reconciling config and runtime", true, false, true
	}
	return "", false, false, false
}

func (m *Model) beginTransientDraft() {
	if m.draft != nil {
		m.transientDraft = false
		return
	}
	if m.ensureDraft() {
		m.transientDraft = true
	}
}

func (m *Model) prepareWrite() bool {
	if m.operation != operationIdle {
		m.setStatus("wait for the current operation to finish")
		return false
	}
	if m.configBarrierID != 0 {
		m.setStatus("wait for the shared configuration refresh to finish")
		return false
	}
	if m.sync == syncStale {
		m.setStatus("local draft is stale; discard it before editing")
		return false
	}
	if !m.canWrite() {
		m.setStatus(m.writeDeniedReason())
		return false
	}
	return true
}

func (m Model) editableRules() []RuleInfo {
	if m.draft != nil {
		return cloneRules(m.draft.Rules)
	}
	if m.config != nil {
		return cloneRules(m.config.Rules)
	}
	return nil
}

func (m Model) isDeleted(ruleID string) bool {
	if m.draft == nil {
		return false
	}
	_, ok := m.draft.Deleted[ruleID]
	return ok
}

func (m Model) handleEditorKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.editor == nil {
		m.view = viewRules
		return m, nil
	}
	switch msg.String() {
	case "f1":
		m.overlayYOffset = 0
		m.overlay = overlayHelp
		return m, nil
	case "ctrl+c":
		if m.editor.dirty() {
			m.overlay = overlayCancelEditor
			return m, nil
		}
		m.editor = nil
		m.view = viewRules
		return m.cancelTransientDraft()
	case "esc":
		if m.editor.dirty() {
			m.overlay = overlayCancelEditor
			return m, nil
		}
		m.editor = nil
		m.view = viewRules
		return m.cancelTransientDraft()
	case "ctrl+s":
		rule, ok := m.editor.rule()
		if !ok {
			m.setStatus("fix the highlighted fields before saving")
			return m, nil
		}
		m.stageEditorRule(rule)
		return m, nil
	default:
		cmd := m.editor.update(msg)
		return m, cmd
	}
}

func (m *Model) stageEditorRule(rule RuleInfo) {
	if m.editor == nil || !m.ensureDraft() {
		return
	}
	switch m.editor.kind {
	case editorEdit:
		for index := range m.draft.Rules {
			if m.draft.Rules[index].RuleID == m.editor.originalID {
				m.draft.Rules[index] = rule
				break
			}
		}
	default:
		m.draft.Rules = append(m.draft.Rules, rule)
	}
	m.selectedRuleID = rule.RuleID
	m.transientDraft = false
	m.editor = nil
	m.view = viewRules
	m.refreshDraftState()
	m.setStatus("rule saved to local draft")
}

func (m *Model) stageToggle() {
	rule := m.selectedRule()
	if rule == nil {
		m.setStatus("select a rule first")
		return
	}
	if m.isDeleted(rule.RuleID) || !m.ensureDraft() {
		m.setStatus("rule is staged for deletion")
		return
	}
	for index := range m.draft.Rules {
		if m.draft.Rules[index].RuleID == rule.RuleID {
			m.draft.Rules[index].Enabled = !m.draft.Rules[index].Enabled
			state := "disabled"
			if m.draft.Rules[index].Enabled {
				state = "enabled"
			}
			m.refreshDraftState()
			m.setStatus("rule staged as " + state)
			return
		}
	}
}

func (m *Model) stageDelete() {
	rule := m.selectedRule()
	if rule == nil || !m.ensureDraft() {
		return
	}
	baseline := m.baselineRule(rule.RuleID)
	filtered := make([]RuleInfo, 0, len(m.draft.Rules)-1)
	for _, candidate := range m.draft.Rules {
		if candidate.RuleID != rule.RuleID {
			filtered = append(filtered, candidate)
		}
	}
	m.draft.Rules = filtered
	if baseline != nil {
		m.draft.Deleted[rule.RuleID] = *rule
	}
	m.refreshDraftState()
	m.setStatus("rule staged for deletion")
}

func (m Model) baselineRule(ruleID string) *RuleInfo {
	if m.config == nil {
		return nil
	}
	for index := range m.config.Rules {
		if m.config.Rules[index].RuleID == ruleID {
			return &m.config.Rules[index]
		}
	}
	return nil
}

func (m *Model) openUDPSettings() {
	value := 0
	if m.draft != nil {
		value = m.draft.UDPMaxSessions
	} else if m.config != nil {
		value = m.config.UDPMaxSessions
	}
	m.udpInput.SetValue(strconv.Itoa(value))
	m.udpInput.CursorEnd()
	m.udpInput.Focus()
	m.udpInputError = ""
	m.overlay = overlayUDPSettings
}

func (m *Model) stageUDPSettings() bool {
	value, err := strconv.Atoi(strings.TrimSpace(m.udpInput.Value()))
	if err != nil || value < 0 || value > engine.MaxUDPGlobalMaxSessions {
		m.udpInputError = fmt.Sprintf("enter 0-%d (0 uses the server default)", engine.MaxUDPGlobalMaxSessions)
		return false
	}
	if !m.ensureDraft() {
		return false
	}
	m.draft.UDPMaxSessions = value
	m.transientDraft = false
	m.udpInput.Blur()
	m.overlay = overlayNone
	m.refreshDraftState()
	m.setStatus("UDP session limit saved to local draft")
	return true
}

func (m Model) startPrecheck() (tea.Model, tea.Cmd) {
	if !m.prepareWrite() {
		return m, nil
	}
	if m.draft == nil {
		m.setStatus("no local changes to precheck")
		return m, nil
	}
	m.operation = operationPrechecking
	m.setStatus("prechecking local draft...")
	return m, precheckCmd(m.ctx, m.client, m.draft.BaseETag, m.draft.request())
}

func (m Model) handlePrecheckResult(msg precheckMsg) (tea.Model, tea.Cmd) {
	m.operation = operationIdle
	if msg.err != nil {
		if apiStatus(msg.err) == http.StatusPreconditionFailed {
			m.sync = syncStale
			m.setStatus("precheck conflict: server configuration changed")
		} else {
			m.setStatus("precheck failed: " + msg.err.Error())
		}
		return m, nil
	}
	if m.draft == nil {
		return m, nil
	}
	if m.sync == syncStale || m.remoteRevision != "" {
		m.setStatus("precheck result ignored: server configuration changed")
		return m, nil
	}
	if msg.resp == nil {
		m.setStatus("precheck returned an empty response")
		return m, nil
	}
	if msg.resp.Revision != "" && msg.resp.Revision != m.draft.BaseRevision {
		m.remoteRevision = msg.resp.Revision
		m.sync = syncStale
		m.setStatus("precheck conflict: server configuration changed")
		return m, nil
	}
	m.precheckResult = msg.resp
	m.resultSelected = 0
	m.view = viewPrecheck
	if msg.resp.Precheck.OK {
		m.sync = syncValidated
		m.setStatus(fmt.Sprintf("precheck passed with %d warning(s)", msg.resp.Precheck.WarningCount))
	} else {
		m.sync = syncDirty
		m.setStatus(fmt.Sprintf("precheck found %d error(s)", msg.resp.Precheck.ErrorCount))
	}
	return m, nil
}

func (m Model) handlePrecheckKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := 0
	if m.precheckResult != nil {
		items = len(m.precheckResult.Precheck.Items)
	}
	switch msg.String() {
	case "q", "ctrl+c":
		if m.draft != nil {
			m.overlay = overlayQuitDirty
			return m, nil
		}
		return m, tea.Quit
	case "?", "f1":
		m.overlayYOffset = 0
		m.overlay = overlayHelp
	case "esc":
		m.view = viewRules
	case "up", "k":
		m.resultSelected = max(0, m.resultSelected-1)
	case "down", "j":
		m.resultSelected = min(max(items-1, 0), m.resultSelected+1)
	case "pgup":
		m.resultSelected = max(0, m.resultSelected-m.precheckPageSize())
	case "pgdown":
		m.resultSelected = min(max(items-1, 0), m.resultSelected+m.precheckPageSize())
	case "home":
		m.resultSelected = 0
	case "end":
		m.resultSelected = max(items-1, 0)
	case "enter", "e":
		if !m.prepareWrite() {
			return m, nil
		}
		if m.precheckResult == nil || m.resultSelected >= items {
			return m, nil
		}
		ruleID := m.precheckResult.Precheck.Items[m.resultSelected].RuleID
		if ruleID == "" {
			return m, nil
		}
		m.selectedRuleID = ruleID
		rule := m.selectedRule()
		if rule != nil && !m.isDeleted(rule.RuleID) {
			m.editor = newRuleEditor(editorEdit, *rule, m.editableRules())
			m.view = viewEditor
		}
	case "A":
		if m.operation != operationIdle {
			m.setStatus("wait for the current operation to finish")
		} else if m.sync == syncValidated && m.precheckResult != nil && m.precheckResult.Precheck.OK {
			m.overlay = overlayApply
		}
	case "P":
		return m.startPrecheck()
	}
	return m, nil
}

func (m Model) handleApplyResult(msg applyMsg) (tea.Model, tea.Cmd) {
	m.operation = operationIdle
	if msg.err != nil {
		message, reconcile, committed, handled := rulesApplyUnavailableState(msg.err)
		if handled {
			m.precheckResult = nil
			m.applyResult = nil
			m.view = viewRules
			if committed {
				m.draft = nil
				m.transientDraft = false
				m.remoteRevision = ""
				m.sync = syncUnavailable
				m.reconcileSelection()
			} else if reconcile {
				if m.draft != nil {
					m.sync = syncStale
				} else {
					m.sync = syncUnavailable
				}
			} else if m.sync != syncStale {
				m.sync = syncDirty
			}
			m.setStatus(message)
			if reconcile {
				requestID := m.beginConfigBarrier()
				statsRequestID := m.beginStatsRequest()
				rulesRequestID := m.beginRulesRequest()
				return m, tea.Batch(
					fetchStatsCmd(m.ctx, m.client, statsRequestID),
					fetchRulesCmd(m.ctx, m.client, rulesRequestID),
					fetchConfigRulesCmd(m.ctx, m.client, requestID),
				)
			}
			return m, nil
		}
		if m.sync == syncStale && apiStatus(msg.err) != http.StatusPreconditionFailed {
			m.setStatus("apply failed after server configuration changed; draft retained")
			m.view = viewRules
			return m, nil
		}
		switch apiStatus(msg.err) {
		case http.StatusPreconditionFailed:
			m.sync = syncStale
			m.setStatus("apply conflict: server configuration changed; draft retained")
		case http.StatusForbidden:
			m.setStatus("apply denied: admin role required; draft retained")
		case http.StatusUnprocessableEntity:
			if apiErr, ok := msg.err.(*APIError); ok {
				var checked PrecheckResponse
				if json.Unmarshal(apiErr.Body, &checked) == nil &&
					(checked.Precheck.ErrorCount > 0 || len(checked.Precheck.Items) > 0) {
					m.precheckResult = &checked
					m.resultSelected = 0
					m.sync = syncDirty
					m.view = viewPrecheck
					m.setStatus("apply blocked by a new precheck finding; draft retained")
					return m, nil
				}
			}
			m.sync = syncDirty
			m.setStatus("apply blocked by precheck; draft retained")
		default:
			if m.sync != syncStale {
				m.sync = syncDirty
			}
			m.setStatus("apply failed: " + msg.err.Error() + "; draft retained")
		}
		m.view = viewRules
		return m, nil
	}
	if msg.resp == nil {
		m.setStatus("apply returned an empty response; draft retained")
		return m, nil
	}
	m.applyResult = msg.resp
	nextConfig := msg.resp.Snapshot()
	if nextConfig != nil && !nextConfig.Writable && m.config != nil {
		nextConfig.Writable = m.config.Writable
	}
	m.config = nextConfig
	m.draft = nil
	m.precheckResult = nil
	m.sync = syncClean
	m.remoteRevision = ""
	m.configRequestID++
	m.lastConfigResponseID = m.configRequestID
	m.view = viewApplyResult
	m.resultSelected = 0
	m.reconcileSelection()
	m.setStatus(fmt.Sprintf("apply complete: %d rule(s) changed", msg.resp.Result.AppliedRules+msg.resp.Result.StoppedRules))
	statsRequestID := m.beginStatsRequest()
	rulesRequestID := m.beginRulesRequest()
	return m, tea.Batch(
		fetchStatsCmd(m.ctx, m.client, statsRequestID),
		fetchRulesCmd(m.ctx, m.client, rulesRequestID),
	)
}

func (m Model) handleOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.overlay == overlayHelp {
		switch key {
		case "esc", "n", "?", "f1", "enter":
			m.overlay = overlayNone
			m.overlayYOffset = 0
		case "up", "k":
			m.scrollHelp(-1)
		case "down", "j":
			m.scrollHelp(1)
		case "pgup":
			m.scrollHelp(-m.helpViewportHeight())
		case "pgdown":
			m.scrollHelp(m.helpViewportHeight())
		case "home":
			m.overlayYOffset = 0
		case "end":
			m.overlayYOffset = m.helpMaxYOffset()
		}
		return m, nil
	}
	if m.overlay == overlayUDPSettings {
		switch key {
		case "esc":
			m.udpInput.Blur()
			m.overlay = overlayNone
			return m.cancelTransientDraft()
		case "enter", "ctrl+s":
			m.stageUDPSettings()
			return m, nil
		default:
			var cmd tea.Cmd
			m.udpInput, cmd = m.udpInput.Update(msg)
			m.udpInputError = ""
			return m, cmd
		}
	}
	if key == "esc" || key == "n" {
		m.overlay = overlayNone
		return m, nil
	}
	switch m.overlay {
	case overlayReload:
		if (key == "enter" || key == "y") && m.operation == operationIdle {
			m.operation = operationReloading
			return m, reloadCmd(m.ctx, m.client)
		}
	case overlayDelete:
		if key == "enter" || key == "y" {
			m.stageDelete()
			m.overlay = overlayNone
		}
	case overlayApply:
		if key == "enter" || key == "y" {
			if m.operation != operationIdle {
				m.overlay = overlayNone
				m.setStatus("wait for the current operation to finish")
				return m, nil
			}
			if m.draft == nil || m.sync != syncValidated {
				m.overlay = overlayNone
				m.setStatus("draft must pass precheck before apply")
				return m, nil
			}
			m.overlay = overlayNone
			m.operation = operationApplying
			m.setStatus("applying local draft...")
			return m, applyCmd(m.ctx, m.client, m.draft.BaseETag, m.draft.request())
		}
	case overlayDiscardDraft:
		if key == "enter" || key == "y" {
			wasStale := m.sync == syncStale
			m.discardDraft()
			m.overlay = overlayNone
			if wasStale {
				m.sync = syncUnavailable
				m.setStatus("local draft discarded; refreshing server configuration")
				requestID := m.beginConfigBarrier()
				return m, fetchConfigRulesCmd(m.ctx, m.client, requestID)
			}
		}
	case overlayQuitDirty:
		if key == "enter" || key == "y" {
			return m, tea.Quit
		}
	case overlayCancelEditor:
		if key == "enter" || key == "y" {
			m.editor = nil
			m.view = viewRules
			m.overlay = overlayNone
			return m.cancelTransientDraft()
		}
	case overlayCancelBotEditor:
		if key == "enter" || key == "y" {
			m.botEditor = nil
			m.botRebasePending = false
			m.view = viewBotConfig
			m.overlay = overlayNone
		}
	case overlayBotStop:
		if key == "enter" || key == "y" {
			if m.botOperation != botOperationIdle {
				m.overlay = overlayNone
				m.setStatus("wait for the current bot operation to finish")
				return m, nil
			}
			m.overlay = overlayNone
			requestID := m.beginBotOperation(botOperationStopping)
			return m, stopBotCmd(m.ctx, m.client, requestID)
		}
	}
	return m, nil
}

func (m *Model) discardDraft() {
	m.draft = nil
	m.precheckResult = nil
	m.applyResult = nil
	m.remoteRevision = ""
	m.transientDraft = false
	if m.config != nil {
		m.sync = syncClean
	} else {
		m.sync = syncUnavailable
	}
	m.view = viewRules
	m.reconcileSelection()
	m.setStatus("local draft discarded")
}

func (m Model) cancelTransientDraft() (tea.Model, tea.Cmd) {
	if !m.transientDraft {
		return m, nil
	}
	wasStale := m.sync == syncStale
	m.draft = nil
	m.transientDraft = false
	m.remoteRevision = ""
	if m.config != nil {
		m.sync = syncClean
	} else {
		m.sync = syncUnavailable
	}
	if !wasStale {
		return m, nil
	}
	m.sync = syncUnavailable
	m.setStatus("local draft discarded; refreshing server configuration")
	requestID := m.beginConfigBarrier()
	return m, fetchConfigRulesCmd(m.ctx, m.client, requestID)
}

func (m Model) localDiff() localRuleDiff {
	if m.draft == nil || m.config == nil {
		return localRuleDiff{}
	}
	base := make(map[string]RuleInfo, len(m.config.Rules))
	next := make(map[string]RuleInfo, len(m.draft.Rules))
	for _, rule := range m.config.Rules {
		base[rule.RuleID] = rule
	}
	for _, rule := range m.draft.Rules {
		next[rule.RuleID] = rule
	}
	diff := localRuleDiff{}
	for _, rule := range m.draft.Rules {
		previous, ok := base[rule.RuleID]
		if !ok {
			diff.Added = append(diff.Added, rule.RuleID)
			continue
		}
		if reflect.DeepEqual(previous, rule) {
			continue
		}
		left, right := previous, rule
		left.Enabled = right.Enabled
		if reflect.DeepEqual(left, right) {
			if rule.Enabled {
				diff.Enabled = append(diff.Enabled, rule.RuleID)
			} else {
				diff.Disabled = append(diff.Disabled, rule.RuleID)
			}
		} else {
			diff.Updated = append(diff.Updated, rule.RuleID)
		}
	}
	for _, rule := range m.config.Rules {
		if _, ok := next[rule.RuleID]; !ok {
			diff.Deleted = append(diff.Deleted, rule.RuleID)
		}
	}
	return diff
}

func (m Model) dirtyChangeCount() int {
	if m.draft == nil {
		return 0
	}
	count := m.localDiff().changeCount()
	if m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		count++
	}
	return count
}

func (m Model) ruleChange(ruleID string) string {
	if m.draft == nil || m.config == nil {
		return ""
	}
	if _, ok := m.draft.Deleted[ruleID]; ok {
		return "- DELETE"
	}
	current := findRule(m.draft.Rules, ruleID)
	baseline := findRule(m.config.Rules, ruleID)
	if current == nil {
		return ""
	}
	if baseline == nil {
		return "+ ADD"
	}
	if reflect.DeepEqual(*baseline, *current) {
		return ""
	}
	left, right := *baseline, *current
	left.Enabled = right.Enabled
	if reflect.DeepEqual(left, right) {
		return "TOGGLE"
	}
	return "~ EDIT"
}

func findRule(rules []RuleInfo, ruleID string) *RuleInfo {
	for index := range rules {
		if rules[index].RuleID == ruleID {
			return &rules[index]
		}
	}
	return nil
}

func (m Model) ruleState(rule RuleInfo) string {
	if m.rulesErr != nil {
		return "UNKNOWN"
	}
	runtimeRule := m.runtimeRule(rule.RuleID)
	baseline := m.baselineRule(rule.RuleID)
	if m.rules == nil {
		if (baseline != nil && !baseline.Enabled) || (baseline == nil && !rule.Enabled) {
			return "DISABLED"
		}
		return "PENDING"
	}
	if baseline == nil {
		if m.config == nil && runtimeRule != nil && runtimeRule.RuntimeEqual(rule) {
			return "RUNNING"
		}
		if runtimeRule != nil {
			return "DRIFT"
		}
		if rule.Enabled {
			return "PENDING"
		}
		return "DISABLED"
	}
	if !baseline.Enabled {
		if runtimeRule != nil {
			return "DRIFT"
		}
		return "DISABLED"
	}
	if runtimeRule == nil {
		return "ERROR"
	}
	if !runtimeRule.RuntimeEqual(*baseline) {
		return "DRIFT"
	}
	return "RUNNING"
}

func (m Model) runtimeRule(ruleID string) *RuleInfo {
	if m.rules == nil {
		return nil
	}
	return findRule(m.rules.Items, ruleID)
}

func (m Model) syncLabel() string {
	switch m.sync {
	case syncClean:
		return "SYNCED"
	case syncDirty:
		return fmt.Sprintf("DRAFT %d", m.dirtyChangeCount())
	case syncValidated:
		return fmt.Sprintf("VALIDATED %d", m.dirtyChangeCount())
	case syncStale:
		return "STALE"
	default:
		return "CONFIG UNAVAILABLE"
	}
}

func (m Model) roleLabel() string {
	if m.session == nil {
		return "NO SESSION"
	}
	role := strings.ToUpper(strings.TrimSpace(m.session.Role))
	if role == "" {
		role = "UNKNOWN"
	}
	if !m.canWrite() {
		role += " READ ONLY"
	}
	return role
}

func sortedDiffValues(diff localRuleDiff) []string {
	items := make([]string, 0, diff.changeCount())
	for _, item := range diff.Added {
		items = append(items, "+ add      "+item)
	}
	for _, item := range diff.Updated {
		items = append(items, "~ update   "+item)
	}
	for _, item := range diff.Enabled {
		items = append(items, "+ enable   "+item)
	}
	for _, item := range diff.Disabled {
		items = append(items, "- disable  "+item)
	}
	for _, item := range diff.Deleted {
		items = append(items, "- delete   "+item)
	}
	sort.Strings(items)
	return items
}

func precheckDiffValues(diff []ConfigRuleDiff) []string {
	items := make([]string, 0, len(diff))
	for _, item := range diff {
		line := fmt.Sprintf("%-9s %-20s", item.ConfigAction, item.RuleID)
		if item.RuntimeAction != "" && item.RuntimeAction != "none" {
			line += "  runtime: " + item.RuntimeAction
		}
		items = append(items, line)
	}
	sort.Strings(items)
	return items
}
