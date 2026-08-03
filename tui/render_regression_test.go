package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/i18n"
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
	if !strings.Contains(output, "Rule state unknown") || strings.Contains(output, "No configured rules") {
		t.Fatalf("offline rules rendered a confirmed-empty configuration:\n%s", output)
	}
	for _, misleading := range []string{"0 rules", "UDP max 0", "[enter]detail"} {
		if strings.Contains(output, misleading) {
			t.Fatalf("offline rules rendered unavailable data/action as %q:\n%s", misleading, output)
		}
	}
	if !strings.Contains(output, "[r]efresh") {
		t.Fatalf("offline rules omitted the recovery action:\n%s", output)
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

func TestChineseViewsFitRepresentativeTerminalWidths(t *testing.T) {
	for _, size := range []struct {
		width  int
		height int
	}{{40, 12}, {80, 24}, {120, 32}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := managedTestModelWithTranslator(i18n.New(i18n.SimplifiedChinese))
			m.width = size.width
			m.height = size.height
			m.config.Rules[0].Name = "东京主节点"
			m.config.Rules[1].Name = "上海备用节点"
			m.rules.Items[0].Name = "东京主节点"
			m.botConfig = &BotConfigResponse{BotToken: "123:token", BotChat: 42, Running: true}

			views := []viewMode{viewDashboard, viewRules, viewDetail, viewBotConfig}
			var combined strings.Builder
			for _, view := range views {
				m.view = view
				output := m.View()
				assertRenderBounds(t, output, size.width, size.height)
				combined.WriteString(ansi.Strip(output))
				combined.WriteByte('\n')
			}

			m.overlay = overlayHelp
			help := m.View()
			assertRenderBounds(t, help, size.width, size.height)
			combined.WriteString(ansi.Strip(help))
			text := combined.String()
			if strings.Contains(text, "tui.") || strings.Contains(text, "common.") || strings.Contains(text, "severity.") {
				t.Fatalf("Chinese TUI leaked a translation key:\n%s", text)
			}
			if !strings.Contains(text, "规则") || !strings.Contains(text, "状态") {
				t.Fatalf("Chinese TUI did not render localized labels:\n%s", text)
			}
			if size.width >= 80 && !strings.Contains(text, "◆ 流量") {
				t.Fatalf("Chinese panel title lost its semantic icon:\n%s", text)
			}
		})
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
	for _, hint := range []string{"[n]ew", "[enter]detail", "[s]ort", "[1]dashboard", "[l]anguage", "[q]uit"} {
		if !strings.Contains(footer, hint) {
			t.Fatalf("rules footer omitted %q:\n%s", hint, footer)
		}
	}
	for _, unavailable := range []string{"[P]recheck", "[A]pply", "[u]discard"} {
		if strings.Contains(footer, unavailable) {
			t.Fatalf("clean rules footer advertised unavailable action %q:\n%s", unavailable, footer)
		}
	}

	m.selectedRuleID = "running"
	m.stageToggle()
	footer = ansi.Strip(m.renderFooter())
	if !strings.Contains(footer, "[enter]precheck") || !strings.Contains(footer, "[u]discard") || strings.Contains(footer, "[enter]detail") {
		t.Fatalf("draft footer did not prioritize the next valid action:\n%s", footer)
	}
	m.sync = syncValidated
	m.precheckResult = &PrecheckResponse{Precheck: precheck.Result{OK: true}}
	footer = ansi.Strip(m.renderFooter())
	if !strings.Contains(footer, "[enter]apply") || strings.Contains(footer, "[enter]precheck") {
		t.Fatalf("validated footer did not advance its primary action:\n%s", footer)
	}

	m.view = viewBotConfig
	m.botConfig = &BotConfigResponse{BotToken: "123:token", BotChat: 42, Running: true}
	footer = ansi.Strip(m.renderFooter())
	if !strings.Contains(footer, "[x]stop") || strings.Contains(footer, "[s]start") {
		t.Fatalf("bot footer advertised the wrong lifecycle action:\n%s", footer)
	}
}

func TestLanguageSelectorFitsRepresentativeTerminals(t *testing.T) {
	for _, locale := range []string{i18n.English, i18n.SimplifiedChinese} {
		for _, size := range []struct {
			width  int
			height int
		}{{40, 12}, {80, 24}} {
			t.Run(fmt.Sprintf("%s_%dx%d", locale, size.width, size.height), func(t *testing.T) {
				m := managedTestModelWithTranslator(i18n.New(locale))
				m.width = size.width
				m.height = size.height
				m.overlay = overlayLanguage
				output := m.View()
				assertRenderBounds(t, output, size.width, size.height)
				plain := ansi.Strip(output)
				for _, expected := range []string{"Language / 语言", "English (en)", "zh-CN", "[Esc]", "╰"} {
					if !strings.Contains(plain, expected) {
						t.Fatalf("language selector was cropped or incomplete; missing %q:\n%s", expected, plain)
					}
				}
			})
		}
	}
}

func TestChinesePrecheckUsesActionableLocalizedMessage(t *testing.T) {
	m := managedTestModelWithTranslator(i18n.New(i18n.SimplifiedChinese))
	m.view = viewPrecheck
	m.selectedRuleID = "running"
	m.stageToggle()
	m.precheckResult = &PrecheckResponse{Precheck: precheck.Result{
		OK: false, ErrorCount: 1, CheckedRules: 2,
		Items: []precheck.Item{{
			Severity: precheck.SeverityError,
			Check:    "listen_bind",
			RuleID:   "running",
			Message:  "cannot bind tcp 0.0.0.0:2201: address already in use",
		}},
	}}

	output := ansi.Strip(m.renderPrecheck())
	if !strings.Contains(output, "监听端口") || !strings.Contains(output, "端口可能已被占用") || strings.Contains(output, "cannot bind") {
		t.Fatalf("Chinese precheck did not present an actionable localized finding:\n%s", output)
	}
	updated, _ := m.handlePrecheckKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = updated.(Model)
	if m.view != viewEditor || m.editor == nil || m.editor.fields[m.editor.focus].key != fieldListenPort {
		t.Fatalf("precheck edit did not focus the listen port field: view=%v editor=%+v", m.view, m.editor)
	}
}

func TestFilterIncludesEndpointsAndRemarks(t *testing.T) {
	m := managedTestModel()
	m.config.Rules[0].TargetAddr = "edge.internal"
	m.config.Rules[1].Remark = "backup route"

	m.filterInput.SetValue("edge.internal")
	if rules := m.filteredRules(); len(rules) != 1 || rules[0].RuleID != "running" {
		t.Fatalf("target search result = %+v", rules)
	}
	m.filterInput.SetValue("backup route")
	if rules := m.filteredRules(); len(rules) != 1 || rules[0].RuleID != "disabled" {
		t.Fatalf("remark search result = %+v", rules)
	}
}

func TestIdleHeaderDoesNotRenderBusySpinner(t *testing.T) {
	m := managedTestModel()
	m.stats = &StatsResponse{}
	m.lastStatsAt = time.Time{}
	if activity := ansi.Strip(m.renderHeaderActivity()); activity != "" {
		t.Fatalf("idle header activity = %q, want empty", activity)
	}
	header := ansi.Strip(m.renderHeader())
	for _, tab := range []string{"[1 DASHBOARD]", "[2 RULES]", "[3 BOT]"} {
		if !strings.Contains(header, tab) {
			t.Fatalf("header omitted navigation tab %q: %s", tab, header)
		}
	}
	if strings.Contains(header, "[4 NET]") {
		t.Fatalf("header retained standalone network tab: %s", header)
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
	if strings.Count(output, "╰") != 2 || !strings.Contains(output, "Forwarding Traffic") || !strings.Contains(output, "Host Network") {
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

func TestDetailShowsSourceIPPolicyAndDenials(t *testing.T) {
	m := managedTestModel()
	m.width = 90
	m.height = 30
	m.view = viewDetail
	for index := range m.config.Rules {
		if m.config.Rules[index].RuleID == m.selectedRuleID {
			m.config.Rules[index].SourceIPMode = engine.SourceIPModeDenylist
			m.config.Rules[index].SourceIPs = []string{"192.0.2.0/24"}
		}
	}
	m.stats = &StatsResponse{Items: []TrafficSnapshot{{RuleID: m.selectedRuleID, SourceIPDenied: 7}}}
	output := ansi.Strip(m.renderDetail())
	for _, want := range []string{"IP Access", "denylist", "192.0.2.0/24", "IP Denied", "7"} {
		if !strings.Contains(output, want) {
			t.Fatalf("detail omitted %q:\n%s", want, output)
		}
	}
}

func TestDetailShowsSharedBandwidthAndQuotaStatus(t *testing.T) {
	m := managedTestModel()
	m.width = 100
	m.height = 40
	m.view = viewDetail
	for index := range m.config.Rules {
		if m.config.Rules[index].RuleID != m.selectedRuleID {
			continue
		}
		m.config.Rules[index].Bandwidth = &engine.BandwidthLimit{
			Upload:   engine.ByteRate(12_500_000),
			Download: engine.ByteRate(25_000_000),
		}
		m.config.Rules[index].TrafficQuota = &engine.TrafficQuota{
			Daily: engine.ByteSize(100_000_000_000), Monthly: engine.ByteSize(1_000_000_000_000),
			Timezone: "Asia/Shanghai", Action: "block",
		}
	}
	m.stats = &StatsResponse{Items: []TrafficSnapshot{{
		RuleID: m.selectedRuleID, QuotaRejectedBytes: 1024,
		Quota: &engine.TrafficQuotaSnapshot{
			Blocked: true,
			Daily: &engine.QuotaPeriodSnapshot{
				Period: "2026-08-02", UsedBytes: 100_000_000_000, LimitBytes: 100_000_000_000,
				RemainingBytes: 0, ResetTime: time.Date(2026, 8, 3, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).Unix(), Exhausted: true,
			},
			Monthly: &engine.QuotaPeriodSnapshot{
				Period: "2026-08", UsedBytes: 100_000_000_000, LimitBytes: 1_000_000_000_000,
				RemainingBytes: 900_000_000_000, ResetTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).Unix(),
			},
		},
	}}}
	output := ansi.Strip(m.renderDetail())
	for _, want := range []string{
		"Rule Upload", "100 Mbps", "Rule Download", "200 Mbps",
		"Quota Status", "blocked", "Daily Quota", "100 GB", "exhausted",
		"Monthly Quota", "1 TB", "Quota Reject", "1.0K",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("detail omitted %q:\n%s", want, output)
		}
	}
}

func TestDetailShowsUnknownWhenQuotaSnapshotIsMissing(t *testing.T) {
	m := managedTestModel()
	m.width = 100
	m.height = 40
	m.view = viewDetail
	for index := range m.config.Rules {
		if m.config.Rules[index].RuleID == m.selectedRuleID {
			m.config.Rules[index].TrafficQuota = &engine.TrafficQuota{
				Daily: 100_000_000_000, Monthly: 1_000_000_000_000,
				Timezone: "Asia/Shanghai", Action: "block",
			}
		}
	}
	m.stats = &StatsResponse{Items: []TrafficSnapshot{{RuleID: m.selectedRuleID}}}
	output := ansi.Strip(m.renderDetail())
	for _, label := range []string{"Quota Status", "Daily Quota", "Monthly Quota"} {
		found := false
		for _, line := range strings.Split(output, "\n") {
			if !strings.Contains(line, label) {
				continue
			}
			found = true
			if !strings.Contains(line, "unknown") || strings.Contains(line, "active") {
				t.Fatalf("%s line did not report unknown state: %q\n%s", label, line, output)
			}
		}
		if !found {
			t.Fatalf("detail omitted %q:\n%s", label, output)
		}
	}
}

func TestWideRulesShowSourceIPPolicySummary(t *testing.T) {
	m := managedTestModel()
	m.width = 160
	m.height = 30
	m.view = viewRules
	for index := range m.config.Rules {
		if m.config.Rules[index].RuleID == "running" {
			m.config.Rules[index].SourceIPMode = engine.SourceIPModeAllowlist
			m.config.Rules[index].SourceIPs = []string{"127.0.0.1", "192.0.2.0/24"}
		}
	}
	output := ansi.Strip(m.renderRules())
	if !strings.Contains(output, "ACCESS") || !strings.Contains(output, "ALLOW 2") {
		t.Fatalf("wide rules omitted source policy summary:\n%s", output)
	}
}

func TestCompactSourceIPPolicyDetailStaysInBounds(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewDetail
	for index := range m.config.Rules {
		if m.config.Rules[index].RuleID == m.selectedRuleID {
			m.config.Rules[index].SourceIPMode = engine.SourceIPModeAllowlist
			m.config.Rules[index].SourceIPs = []string{"2001:db8:100::/48", "192.0.2.0/24"}
		}
	}
	output := m.View()
	if !strings.Contains(ansi.Strip(output), m.selectedRuleID) {
		t.Fatalf("compact detail lost selected rule:\n%s", output)
	}
	assertRenderBounds(t, output, m.width, m.height)
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

func TestChineseApplyResultLocalizesRuntimeOutcome(t *testing.T) {
	m := managedTestModelWithTranslator(i18n.New(i18n.SimplifiedChinese))
	m.width = 80
	m.height = 24
	m.view = viewApplyResult
	m.applyResult = &ApplyResponse{
		Revision: "sha256:55c0b53c89e7ad03c0a6076557008e421b1bf17182e4e7d0cb76546d7f40c536",
		Result: engine.ApplyResult{
			TotalRules: 1,
			Items: []engine.ApplyItemResult{{
				RuleID: "qa-disabled",
				Action: engine.ApplyActionUnchanged,
				Status: "ok",
			}},
		},
	}
	output := ansi.Strip(m.View())
	for _, want := range []string{"运行变更", "运行未变", "成功"} {
		if !strings.Contains(output, want) {
			t.Fatalf("Chinese apply result omitted %q:\n%s", want, output)
		}
	}
	for _, leaked := range []string{"unchanged", " ok"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("Chinese apply result leaked %q:\n%s", leaked, output)
		}
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}

func TestCompactChineseApplyStatusFitsWithoutTruncation(t *testing.T) {
	m := managedTestModelWithTranslator(i18n.New(i18n.SimplifiedChinese))
	m.width = 40
	m.height = 12
	m.view = viewApplyResult
	m.applyResult = &ApplyResponse{Result: engine.ApplyResult{TotalRules: 1}}
	m.setStatus(m.tf("tui.status.apply_complete", 1, 0))

	output := ansi.Strip(m.View())
	want := "配置已保存：1 项配置更改，0 条运行变更"
	if !strings.Contains(output, want) {
		t.Fatalf("compact apply status was truncated, want %q:\n%s", want, output)
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

func TestCrowdedDashboardKeepsAllSectionsScrollable(t *testing.T) {
	m := managedTestModel()
	m.width = 60
	m.height = 20
	for _, id := range []string{"third", "fourth", "fifth", "sixth"} {
		m.config.Rules = append(m.config.Rules, testRule(id, true, 2300+len(m.config.Rules)))
	}
	m.view = viewDashboard
	full := ansi.Strip(m.renderDashboard())
	for _, expected := range []string{"Forwarding Traffic", "Host Network", "Rules", "running"} {
		if !strings.Contains(full, expected) {
			t.Fatalf("crowded dashboard omitted %q:\n%s", expected, full)
		}
	}
	if m.contentMaxYOffset() == 0 {
		t.Fatalf("crowded dashboard unexpectedly fits without scrolling:\n%s", full)
	}
	top := m.View()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(Model)
	bottom := m.View()
	if top == bottom {
		t.Fatal("crowded dashboard did not scroll")
	}
	assertRenderBounds(t, bottom, m.width, m.height)
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
