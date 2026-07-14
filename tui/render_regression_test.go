package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

func TestPanelDividerFitsPanelInnerWidth(t *testing.T) {
	panelWidth := calcFullWidth(60)
	divider := panelDivider(panelWidth, 0)
	if got, want := lipgloss.Width(divider), panelInnerWidth(panelWidth); got != want {
		t.Fatalf("divider width = %d, want %d", got, want)
	}
	rendered := panelStyle.Width(panelWidth).Render(divider)
	if got := lipgloss.Height(rendered); got != 3 {
		t.Fatalf("divider wrapped inside panel: height=%d\n%s", got, rendered)
	}
}

func TestFormatEndpointBracketsIPv6(t *testing.T) {
	if got, want := formatEndpoint("2001:db8::1", 443), "[2001:db8::1]:443"; got != want {
		t.Fatalf("formatEndpoint = %q, want %q", got, want)
	}
}

func TestRulesFilterReportsResultCountAndNoMatches(t *testing.T) {
	m := managedTestModel()
	m.filterInput.SetValue("running")
	output := ansi.Strip(m.renderRules())
	if !strings.Contains(output, "Showing 1 of 2") {
		t.Fatalf("filtered rules omitted count:\n%s", output)
	}

	m.filterInput.SetValue("does-not-exist")
	output = ansi.Strip(m.renderRules())
	if !strings.Contains(output, "Showing 0 of 2") || !strings.Contains(output, "No matches") {
		t.Fatalf("empty filter result is ambiguous:\n%s", output)
	}
	if strings.Contains(output, "No configured rules") {
		t.Fatalf("filter miss was rendered as an empty configuration:\n%s", output)
	}
}

func TestOfflineWithoutCachedDataUsesUnavailableState(t *testing.T) {
	m := newModel(t.Context(), "http://127.0.0.1:19090")
	m.ready = true
	m.width = 80
	m.height = 24
	m.connectionErr = errors.New("dial tcp: connection refused")
	m.rulesErr = m.connectionErr
	m.configErr = m.connectionErr

	output := ansi.Strip(m.View())
	if !strings.Contains(output, "Daemon unavailable") || strings.Contains(output, "Total Upload") {
		t.Fatalf("offline dashboard rendered zero-value telemetry:\n%s", output)
	}

	m.view = viewRules
	output = ansi.Strip(m.View())
	if !strings.Contains(output, "Rules unavailable") || strings.Contains(output, "No configured rules") {
		t.Fatalf("offline rules rendered a confirmed-empty configuration:\n%s", output)
	}
}

func TestCellWidthTruncationHandlesCJK(t *testing.T) {
	got := truncate("东京节点-alpha", 8)
	if width := lipgloss.Width(got); width > 8 {
		t.Fatalf("truncate width = %d, want <= 8: %q", width, got)
	}
	if !strings.HasSuffix(got, "..") {
		t.Fatalf("truncate did not mark clipped value: %q", got)
	}
	if width := lipgloss.Width(cellColumn("东京", 7)); width != 7 {
		t.Fatalf("cellColumn width = %d, want 7", width)
	}
}

func TestEditorInputsRespectAvailableWidth(t *testing.T) {
	rule := testRule("running", true, 2201)
	rule.Name = strings.Repeat("long-name-", 8)
	editor := newRuleEditor(editorEdit, rule, []RuleInfo{rule})
	if got := lipgloss.Width(editor.fields[1].inputView(9)); got > 9 {
		t.Fatalf("rule input width = %d, want <= 9", got)
	}

	botEditor := newBotConfigEditor(&BotConfigResponse{
		BotToken: strings.Repeat("token", 20), BotChat: 42, BotControlToken: strings.Repeat("admin", 20),
	})
	if got := lipgloss.Width(botEditor.inputView(0, 11)); got > 11 {
		t.Fatalf("bot input width = %d, want <= 11", got)
	}
}

func TestFooterShowsAvailableActionsAndNavigation(t *testing.T) {
	m := managedTestModel()
	m.view = viewRules
	footer := ansi.Strip(m.renderFooter())
	for _, hint := range []string{"[s]ort", "[P]recheck", "[tab]dashboard", "[q]uit"} {
		if !strings.Contains(footer, hint) {
			t.Fatalf("rules footer omitted %q:\n%s", hint, footer)
		}
	}

	m.view = viewBotConfig
	m.botConfig = &BotConfigResponse{BotToken: "123:token", BotChat: 42, Running: true}
	footer = ansi.Strip(m.renderFooter())
	if !strings.Contains(footer, "[x]stop") || strings.Contains(footer, "[s]start") {
		t.Fatalf("bot footer advertised the wrong lifecycle action:\n%s", footer)
	}
}

func TestTooSmallTerminalUsesDedicatedView(t *testing.T) {
	m := managedTestModel()
	m.width = 39
	m.height = 11
	output := m.View()
	if !strings.Contains(ansi.Strip(output), "Terminal too small") {
		t.Fatalf("small terminal did not use dedicated view:\n%s", output)
	}
	assertRenderBounds(t, output, m.width, m.height)
}

func TestCompactDashboardAndRulesKeepCompletePanels(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewDashboard
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "Overview") || strings.Count(output, "╰") != 1 {
		t.Fatalf("40x12 dashboard panel was cropped:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)

	m.view = viewRules
	output = ansi.Strip(m.View())
	if !strings.Contains(output, "UDP max") || strings.Count(output, "╰") != 1 {
		t.Fatalf("40x12 rules panel was cropped:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)

	m.width = 60
	m.height = 20
	m.view = viewDashboard
	output = ansi.Strip(m.View())
	if strings.Count(output, "╰") != 2 || !strings.Contains(output, "disabled") {
		t.Fatalf("60x20 dashboard panels were cropped or wrapped:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestWrappedDetailValuesAlignWithValueColumn(t *testing.T) {
	output := ansi.Strip(kvWrapped("Remark", "Disabled IPv6 formatting and responsive layout fixture", 54))
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], strings.Repeat(" ", 17)) {
		t.Fatalf("wrapped value is not aligned with the value column: %q", output)
	}
	output = ansi.Strip(kvWrapped("Rule ID", strings.Repeat("x", 48), 34))
	lines = strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], strings.Repeat(" ", 17)) {
		t.Fatalf("unbroken value is not aligned with the value column: %q", output)
	}
}

func TestDetailHidesZeroOnlyRateHistory(t *testing.T) {
	m := managedTestModel()
	m.width = 80
	m.height = 24
	m.view = viewDetail
	rule := m.selectedRule()
	if rule == nil {
		t.Fatal("managed model has no selected rule")
	}
	m.history[rule.RuleID] = &rateHistory{
		uploadRates:   []float64{0, 0},
		downloadRates: []float64{0, 0},
	}
	if output := ansi.Strip(m.renderDetail()); strings.Contains(output, "Upload rate") {
		t.Fatalf("zero-only history rendered an empty sparkline:\n%s", output)
	}
	m.history[rule.RuleID].uploadRates[1] = 1
	if output := ansi.Strip(m.renderDetail()); !strings.Contains(output, "Upload rate") {
		t.Fatalf("positive history did not render a sparkline:\n%s", output)
	}
}

func TestCompactPrecheckKeepsFindingsVisible(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewPrecheck
	m.precheckResult = &PrecheckResponse{Precheck: precheck.Result{
		ErrorCount:   1,
		CheckedRules: 2,
		Items: []precheck.Item{{
			Severity: precheck.SeverityError,
			RuleID:   "running",
			Check:    "listen",
			Message:  "address is already in use",
		}},
	}}
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "Findings") || !strings.Contains(output, "running") || strings.Count(output, "╰") != 1 {
		t.Fatalf("compact precheck cropped its findings:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestCompactApplyResultKeepsResultsVisible(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewApplyResult
	m.applyResult = &ApplyResponse{
		Revision: "sha256:55c0b53c89e7ad03c0a6076557008e421b1bf17182e4e7d0cb76546d7f40c536",
		Result: engine.ApplyResult{
			AppliedRules: 1,
			TotalRules:   3,
			Items: []engine.ApplyItemResult{{
				RuleID: "dual-stack-staging",
				Action: engine.ApplyActionStarted,
				Status: "ok",
			}},
		},
	}
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "Results") || !strings.Contains(output, "dual-stack") || strings.Count(output, "╰") != 1 {
		t.Fatalf("compact apply result was cropped:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestNarrowStatusShowsActionFeedback(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewRules
	m.setStatus("precheck the current draft first (P)")
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "precheck") {
		t.Fatalf("narrow status hid action feedback:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestDashboardFallsBackBeforeTopRulesAreCropped(t *testing.T) {
	m := managedTestModel()
	m.width = 60
	m.height = 20
	for _, id := range []string{"third", "fourth", "fifth", "sixth"} {
		m.config.Rules = append(m.config.Rules, testRule(id, true, 2300+len(m.config.Rules)))
	}
	m.view = viewDashboard
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "Overview") || !strings.Contains(output, "configured") {
		t.Fatalf("crowded dashboard did not switch to a complete summary:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestCompactApplyConfirmKeepsConfirmationVisible(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.overlay = overlayApply
	m.draft = &draftConfig{
		BaseRevision:   m.config.Revision,
		BaseETag:       m.config.ETag,
		UDPMaxSessions: m.config.UDPMaxSessions + 1,
		Rules:          cloneRules(m.config.Rules),
		Deleted:        make(map[string]RuleInfo),
	}
	for _, id := range []string{"third", "fourth", "fifth"} {
		m.draft.Rules = append(m.draft.Rules, testRule(id, true, 2300+len(m.draft.Rules)))
	}
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "pending change") || !strings.Contains(output, "[Enter/y] apply") {
		t.Fatalf("compact apply confirmation was cropped:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestCompactRuleEditorErrorsStayInsidePanel(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewEditor
	m.editor = newRuleEditor(editorEdit, testRule("running", true, 2201), m.config.Rules)
	m.editor.errors[fieldName] = "required"
	m.editor.errors["form"] = "listen address conflicts with another configured endpoint"
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "required") || !strings.Contains(output, "listen address") {
		t.Fatalf("compact editor omitted validation feedback:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestThemeUsesAdaptiveColors(t *testing.T) {
	if _, ok := any(CText).(lipgloss.AdaptiveColor); !ok {
		t.Fatalf("CText is not adaptive: %T", CText)
	}
	if _, ok := any(CRowA).(lipgloss.AdaptiveColor); !ok {
		t.Fatalf("CRowA is not adaptive: %T", CRowA)
	}
}

func assertRenderBounds(t *testing.T, output string, width, height int) {
	t.Helper()
	lines := strings.Split(output, "\n")
	if len(lines) > height {
		t.Fatalf("render height = %d, want <= %d\n%s", len(lines), height, output)
	}
	for index, line := range lines {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d: %q", index+1, got, width, ansi.Strip(line))
		}
	}
}
