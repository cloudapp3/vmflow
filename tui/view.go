package tui

import (
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/precheck"
)

func (m Model) View() string {
	if !m.ready {
		return m.spinner.View() + " " + m.t("tui.state.initializing")
	}
	if m.width < 40 || m.height < 12 {
		return m.renderTooSmall()
	}
	if m.overlay != overlayNone {
		overlayViewport := m.viewport
		overlayViewport.Width = max(m.width, 1)
		if m.overlay == overlayHelp {
			footer := m.renderHelpFooter()
			overlayViewport.Height = m.helpViewportHeight()
			overlayViewport.SetContent(m.renderOverlay())
			overlayViewport.SetYOffset(m.overlayYOffset)
			return lipgloss.JoinVertical(lipgloss.Left, overlayViewport.View(), footer)
		}
		overlayViewport.Height = max(m.height, 1)
		overlayViewport.SetContent(m.renderOverlay())
		overlayViewport.SetYOffset(0)
		return overlayViewport.View()
	}

	var content string
	switch m.view {
	case viewDashboard:
		content = m.renderDashboard()
	case viewRules:
		content = m.renderRules()
	case viewDetail:
		content = m.renderDetail()
	case viewEditor:
		content = m.renderEditor()
	case viewPrecheck:
		content = m.renderPrecheck()
	case viewApplyResult:
		content = m.renderApplyResult()
	case viewBotConfig:
		content = m.renderBotConfig()
	case viewBotEditor:
		content = m.renderBotEditor()
	}
	header := m.renderHeader()
	status := m.renderStatusLine()
	footer := m.renderFooter()
	contentHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(status)-lipgloss.Height(footer), 1)
	contentViewport := m.viewport
	contentViewport.Width = max(m.width, 1)
	contentViewport.Height = contentHeight
	contentViewport.SetContent(content)
	if m.view == viewDashboard || m.view == viewDetail {
		contentViewport.SetYOffset(m.contentYOffset)
	} else {
		contentViewport.SetYOffset(0)
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		contentViewport.View(),
		status,
		footer,
	)
}

func (m Model) renderTooSmall() string {
	width := max(m.width, 1)
	height := max(m.height, 1)
	lines := []string{
		titleStyle.Render("vmflow"),
		m.t("tui.terminal.too_small"),
		m.tf("tui.terminal.minimum_size", m.width, m.height),
	}
	for index := range lines {
		lines[index] = truncate(lines[index], width)
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
}

func (m Model) renderHeader() string {
	left := lipgloss.JoinHorizontal(lipgloss.Left,
		m.renderBadge(), "  ", titleStyle.Render("vmflow"), "  ", subtleStyle.Render(m.viewName()),
	)
	if m.width >= 72 {
		left = lipgloss.JoinHorizontal(lipgloss.Left,
			m.renderBadge(), "  ", titleStyle.Render("vmflow"), "  ", m.renderNavigation(),
		)
	}
	right := m.renderHeaderActivity()
	if right == "" || lipgloss.Width(left)+lipgloss.Width(right)+1 > m.width {
		return ansi.Truncate(left, max(m.width, 1), "")
	}
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) renderNavigation() string {
	active := viewRules
	switch m.view {
	case viewDashboard:
		active = viewDashboard
	case viewBotConfig, viewBotEditor:
		active = viewBotConfig
	}
	items := []struct {
		key     string
		label   string
		view    viewMode
		enabled bool
	}{
		{key: "1", label: m.t("tui.view.dashboard"), view: viewDashboard, enabled: true},
		{key: "2", label: m.t("tui.view.rules"), view: viewRules, enabled: true},
		{key: "3", label: m.t("tui.view.bot"), view: viewBotConfig, enabled: m.canWrite()},
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		text := fmt.Sprintf("[%s %s]", item.key, item.label)
		switch {
		case item.view == active:
			parts = append(parts, titleStyle.Render(text))
		case item.enabled:
			parts = append(parts, valueStyle.Render(text))
		default:
			parts = append(parts, subtleStyle.Render(text))
		}
	}
	return strings.Join(parts, " ")
}

func (m Model) renderHeaderActivity() string {
	switch {
	case m.serviceActivityLabel() != "":
		return valueStyle.Render(m.serviceActivityLabel())
	case m.operation != operationIdle:
		return valueStyle.Render(m.operationLabel())
	case m.botOperation != botOperationIdle:
		return valueStyle.Render(m.botOperationLabel())
	case m.paused:
		return badgePausedStyle.Render(m.t("tui.state.paused"))
	case m.stats == nil && m.connectionErr == nil:
		return subtleStyle.Render(m.spinner.View() + " " + m.t("tui.state.connecting"))
	case !m.lastStatsAt.IsZero():
		age := timeSince(m.lastStatsAt)
		switch {
		case age < 2*time.Second:
			return subtleStyle.Render(m.t("tui.header.updated_now"))
		case age < time.Minute:
			return subtleStyle.Render(m.tf("tui.header.updated_seconds", int(age.Seconds())))
		default:
			return subtleStyle.Render(m.tf("tui.header.updated_minutes", int(age.Minutes())))
		}
	default:
		return ""
	}
}

func (m Model) viewName() string {
	switch m.view {
	case viewDashboard:
		return m.t("tui.view.dashboard")
	case viewRules:
		return m.t("tui.view.rules")
	case viewDetail:
		return m.t("tui.view.detail")
	case viewEditor:
		return m.t("tui.view.editor")
	case viewPrecheck:
		return m.t("tui.view.precheck")
	case viewApplyResult:
		return m.t("tui.view.apply_result")
	case viewBotConfig:
		return m.t("tui.view.bot")
	case viewBotEditor:
		return m.t("tui.view.bot_editor")
	default:
		return ""
	}
}

func (m Model) renderBadge() string {
	if m.connectionErr != nil {
		return badgeOfflineStyle.Render(m.t("tui.badge.offline"))
	}
	if m.stats == nil {
		return badgePausedStyle.Render(m.t("tui.badge.connecting"))
	}
	return badgeOnlineStyle.Render(m.t("tui.badge.online"))
}

func (m Model) renderStatusLine() string {
	recentStatus := m.statusText != "" && timeSince(m.statusTime) < 8*time.Second
	if m.width < 60 {
		message := ""
		switch {
		case m.operation != operationIdle:
			message = m.operationLabel()
		case m.botOperation != botOperationIdle:
			message = m.botOperationLabel()
		case recentStatus:
			message = m.statusText
		case m.connectionErr != nil:
			message = m.tf("tui.error.connection", m.friendlyError(m.connectionErr))
		case m.configErr != nil:
			message = m.tf("tui.error.config", m.friendlyError(m.configErr))
		case m.rulesErr != nil:
			message = m.tf("tui.error.rules", m.friendlyError(m.rulesErr))
		}
		if message != "" {
			return statusLineStyle.Render(ansi.Truncate(valueStyle.Render(safeText(message)), max(m.width, 1), ".."))
		}
	}
	parts := []string{
		subtleStyle.Render(safeText(m.roleLabel())),
		subtleStyle.Render("|"),
		syncStyle(m.sync).Render(m.syncLabel()),
	}
	if m.view == viewRules {
		direction := "↑"
		if m.sortDesc {
			direction = "↓"
		}
		parts = append(parts, subtleStyle.Render("|"), subtleStyle.Render(m.tf("tui.status.sort", m.localizedSortKey()+" "+direction)))
	}
	if m.operation != operationIdle {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(m.operationLabel()))
	}
	if m.botOperation != botOperationIdle {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(m.botOperationLabel()))
	}
	if m.paused {
		parts = append(parts, badgePausedStyle.Render(m.t("tui.state.paused")))
	}
	if recentStatus {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(truncate(m.statusText, 72)))
	}
	if m.connectionErr != nil {
		parts = append(parts, subtleStyle.Render("|"), badgeOfflineStyle.Render(truncate(m.tf("tui.error.connection", m.friendlyError(m.connectionErr)), 60)))
	} else if m.configErr != nil {
		parts = append(parts, subtleStyle.Render("|"), badgeOfflineStyle.Render(truncate(m.tf("tui.error.config", m.friendlyError(m.configErr)), 60)))
	} else if m.rulesErr != nil {
		parts = append(parts, subtleStyle.Render("|"), badgeOfflineStyle.Render(truncate(m.tf("tui.error.rules", m.friendlyError(m.rulesErr)), 60)))
	}
	return statusLineStyle.MaxWidth(max(m.width, 1)).Render(strings.Join(parts, " "))
}

func (m Model) botOperationLabel() string {
	switch m.botOperation {
	case botOperationFetching:
		return m.spinner.View() + " " + m.t("tui.operation.bot_refresh")
	case botOperationSaving:
		return m.spinner.View() + " " + m.t("tui.operation.bot_save")
	case botOperationStarting:
		return m.spinner.View() + " " + m.t("tui.operation.bot_start")
	case botOperationStopping:
		return m.spinner.View() + " " + m.t("tui.operation.bot_stop")
	default:
		return ""
	}
}

func (m Model) operationLabel() string {
	switch m.operation {
	case operationPrechecking:
		return m.spinner.View() + " " + m.t("tui.operation.prechecking")
	case operationApplying:
		return m.spinner.View() + " " + m.t("tui.operation.applying")
	case operationReloading:
		return m.spinner.View() + " " + m.t("tui.operation.reloading")
	default:
		return ""
	}
}

func syncStyle(state syncState) lipgloss.Style {
	switch state {
	case syncClean:
		return badgeOnlineStyle
	case syncValidated:
		return lipgloss.NewStyle().Bold(true).Foreground(CCyan)
	case syncDirty:
		return badgePausedStyle
	case syncStale:
		return badgeOfflineStyle
	default:
		return subtleStyle
	}
}

func (m Model) renderFooter() string {
	if m.width < 50 || m.height < 16 {
		return m.renderCompactFooter()
	}
	var hints []string
	switch m.view {
	case viewDashboard:
		if m.connectionErr != nil {
			if m.canStartService() {
				hints = append(hints, "[enter]start service")
			}
			hints = append(hints, "[r]refresh", "[l]anguage", "[?]help", "[q]quit")
			break
		}
		hints = append(hints, "[2]rules", "[j/k]scroll", "[pgup/pgdn]page")
		if network := m.hostNetwork(); network != nil && network.Available {
			if m.networkExpanded {
				hints = append(hints, "[a]active")
			} else {
				hints = append(hints, "[a]all")
			}
		}
		hints = append(hints, "[r]efresh", "[p]ause")
		if m.canReload() {
			hints = append(hints, "[R]eload")
		}
		if m.canWrite() {
			hints = append(hints, "[3]bot")
		}
		hints = append(hints, "[l]anguage", "[?]help", "[q]uit")
	case viewRules:
		if m.rulesUnavailable() {
			hints = []string{"[r]refresh", "[1]dashboard", "[l]anguage", "[?]help", "[q]quit"}
			break
		}
		selected := m.selectedRule() != nil
		if m.canWrite() && m.draft != nil {
			switch m.sync {
			case syncValidated:
				hints = append(hints, "[enter]apply")
			case syncStale:
				hints = append(hints, "[u]discard")
			default:
				hints = append(hints, "[enter]precheck")
			}
			if selected {
				hints = append(hints, "[e]dit")
			}
			if m.sync != syncStale {
				hints = append(hints, "[u]discard")
			}
		} else if m.canWrite() {
			hints = append(hints, "[n]ew")
			if selected {
				hints = append(hints, "[e]dit", "[space]toggle")
			}
		}
		if selected && m.draft == nil {
			hints = append(hints, "[enter]detail")
		}
		hints = append(hints, "[/]filter", "[s]ort", "[1]dashboard", "[r]efresh", "[l]anguage", "[?]help", "[q]uit")
	case viewDetail:
		if m.canWrite() {
			hints = append(hints, "[e]dit", "[space]toggle")
		}
		hints = append(hints, "[j/k]scroll", "[esc]rules", "[1]dashboard", "[r]efresh")
		hints = append(hints, "[l]anguage", "[?]help", "[q]uit")
	case viewEditor:
		hints = []string{"[tab/up/down]field", "[left/right]option", "[space]toggle", "[ctrl+s]save", "[F2]advanced", "[esc]cancel", "[F1]help"}
		if m.editor != nil && m.editor.advanced {
			hints[4] = "[F2]basic"
		}
	case viewPrecheck:
		if m.precheckResult != nil && m.precheckResult.Precheck.OK && m.sync == syncValidated {
			hints = append(hints, "[enter]apply")
		}
		if m.precheckResult != nil && len(m.precheckResult.Precheck.Items) > 0 {
			hints = append(hints, "[e]edit")
		}
		hints = append(hints, "[up/down]finding", "[P]recheck", "[esc]rules", "[l]anguage", "[?]help", "[q]uit")
	case viewApplyResult:
		hints = []string{"[up/down]result", "[pgup/pgdn]page", "[enter/esc]rules", "[tab]dashboard", "[p]ause", "[r]efresh"}
		if m.canReload() {
			hints = append(hints, "[R]eload")
		}
		hints = append(hints, "[l]anguage", "[?]help", "[q]uit")
	case viewBotConfig:
		if m.botOperation == botOperationIdle {
			if m.botConfig != nil && m.botConfigErr == nil {
				hints = append(hints, "[e]edit")
			}
			if m.botConfig != nil && m.botConfigErr == nil && m.botConfig.Running {
				hints = append(hints, "[x]stop")
			} else if m.botConfigErr == nil && botReadyToStart(m.botConfig) {
				hints = append(hints, "[s]start")
			}
			hints = append(hints, "[r]refresh")
		}
		hints = append(hints, "[2]rules", "[l]anguage", "[?]help", "[q]uit")
	case viewBotEditor:
		hints = []string{"[tab/shift+tab]field", "[ctrl+s]save", "[ctrl+r]rebase", "[esc]cancel", "[F1]help"}
	}
	return footerStyle.Render(wrapHints(m.localizedHints(hints), max(m.width, 1)))
}

func (m Model) renderCompactFooter() string {
	var hints []string
	switch m.view {
	case viewDashboard:
		if m.connectionErr != nil {
			if m.canStartService() {
				hints = append(hints, "[enter]start service")
			}
			hints = append(hints, "[r]refresh", "[?]help", "[q]quit")
		} else {
			hints = []string{"[2]rules", "[j/k]scroll"}
			if network := m.hostNetwork(); network != nil && network.Available {
				if m.networkExpanded {
					hints = append(hints, "[a]active")
				} else {
					hints = append(hints, "[a]all")
				}
			}
			hints = append(hints, "[r]refresh", "[?]help", "[q]quit")
		}
	case viewRules:
		if m.rulesUnavailable() {
			hints = []string{"[r]refresh", "[1]dashboard", "[?]help"}
			break
		}
		selected := m.selectedRule() != nil
		if m.canWrite() && m.draft != nil {
			if m.sync == syncValidated {
				hints = append(hints, "[enter]apply")
			} else if m.sync == syncStale {
				hints = append(hints, "[u]discard")
			} else {
				hints = append(hints, "[enter]precheck")
			}
			if selected {
				hints = append(hints, "[e]edit")
			}
		} else if m.canWrite() {
			hints = append(hints, "[n]new")
			if selected {
				hints = append(hints, "[e]edit")
			}
		}
		if selected && m.draft == nil {
			hints = append(hints, "[enter]detail")
		}
		hints = append(hints, "[?]help")
	case viewDetail:
		hints = []string{"[j/k]scroll", "[esc]rules", "[?]help"}
	case viewEditor:
		hints = []string{"[tab]field", "[ctrl+s]save", "[F2]advanced", "[esc]cancel"}
		if m.editor != nil && m.editor.advanced {
			hints[2] = "[F2]basic"
		}
	case viewPrecheck:
		if m.precheckResult != nil && m.precheckResult.Precheck.OK && m.sync == syncValidated {
			hints = append(hints, "[enter]apply")
		} else if m.precheckResult != nil && len(m.precheckResult.Precheck.Items) > 0 {
			hints = append(hints, "[e]edit")
		}
		hints = append(hints, "[j/k]finding", "[esc]rules")
	case viewApplyResult:
		hints = []string{"[j/k]result", "[enter]rules", "[?]help"}
	case viewBotConfig:
		if m.botOperation == botOperationIdle {
			if m.botConfig != nil && m.botConfigErr == nil {
				hints = append(hints, "[e]edit")
				if m.botConfig.Running {
					hints = append(hints, "[x]stop")
				} else if botReadyToStart(m.botConfig) {
					hints = append(hints, "[s]start")
				}
			}
			hints = append(hints, "[r]refresh")
		}
		hints = append(hints, "[2]rules", "[?]help")
	case viewBotEditor:
		hints = []string{"[tab]field", "[ctrl+s]save", "[ctrl+r]rebase", "[esc]cancel"}
	}
	return footerStyle.Render(wrapHints(m.localizedHints(hints), max(m.width, 1)))
}

func (m Model) localizedSortKey() string {
	switch m.sort {
	case sortName:
		return m.t("tui.sort.name")
	case sortConns:
		return m.t("tui.sort.connections")
	case sortUpload:
		return m.t("tui.sort.upload")
	case sortDownload:
		return m.t("tui.sort.download")
	default:
		return "?"
	}
}

func (m Model) localizedHints(hints []string) []string {
	keys := map[string]string{
		"[b]ot": "tui.hint.bot", "[tab]rules": "tui.hint.rules", "[p]ause": "tui.hint.pause",
		"[1]dashboard": "tui.hint.dashboard_number", "[2]rules": "tui.hint.rules_number", "[3]bot": "tui.hint.bot_number",
		"[r]efresh": "tui.hint.refresh", "[r]refresh": "tui.hint.refresh", "[R]eload": "tui.hint.reload", "[?]help": "tui.hint.help",
		"[q]uit": "tui.hint.quit", "[q]quit": "tui.hint.quit", "[n]ew": "tui.hint.new",
		"[n]new": "tui.hint.new", "[e]dit": "tui.hint.edit", "[e]edit": "tui.hint.edit_compact",
		"[c]opy": "tui.hint.copy", "[space]toggle": "tui.hint.toggle", "[d]elete": "tui.hint.delete",
		"[g]lobal": "tui.hint.global", "[P]recheck": "tui.hint.precheck", "[P]check": "tui.hint.precheck_short",
		"[A]pply": "tui.hint.apply", "[A]apply": "tui.hint.apply", "[u]discard": "tui.hint.discard",
		"[/]filter": "tui.hint.filter", "[s]ort": "tui.hint.sort", "[enter]detail": "tui.hint.detail",
		"[tab]dashboard": "tui.hint.dashboard", "[j/k]scroll": "tui.hint.scroll", "[pgup/pgdn]page": "tui.hint.page",
		"[home/end]jump": "tui.hint.jump", "[esc]rules": "tui.hint.back_rules", "[tab/up/down]field": "tui.hint.field",
		"[tab]field": "tui.hint.field_short", "[tab/shift+tab]field": "tui.hint.field_cycle", "[left/right]option": "tui.hint.option",
		"[ctrl+s]save": "tui.hint.save", "[esc]cancel": "tui.hint.cancel", "[F1]help": "tui.hint.help_f1",
		"[up/down]finding": "tui.hint.finding", "[j/k]finding": "tui.hint.finding_short", "[enter/e]edit": "tui.hint.edit_finding",
		"[up/down]result": "tui.hint.result", "[j/k]result": "tui.hint.result_short", "[enter/esc]rules": "tui.hint.result_rules",
		"[enter]rules": "tui.hint.result_rules_enter", "[x]stop": "tui.hint.stop", "[s]start": "tui.hint.start",
		"[ctrl+r]rebase": "tui.hint.rebase", "[esc/?/F1]close": "tui.hint.close",
		"[enter]precheck": "tui.hint.precheck_enter", "[enter]apply": "tui.hint.apply_enter",
		"[enter]start service": "tui.hint.start_service",
		"[l]anguage":           "tui.hint.language",
		"[a]all":               "tui.hint.network_all",
		"[a]active":            "tui.hint.network_active",
		"[F2]advanced":         "tui.hint.advanced", "[F2]basic": "tui.hint.basic",
	}
	localized := make([]string, 0, len(hints))
	for _, hint := range hints {
		if key := keys[hint]; key != "" {
			localized = append(localized, m.t(key))
		} else {
			localized = append(localized, hint)
		}
	}
	return localized
}

func wrapHints(hints []string, width int) string {
	if len(hints) == 0 {
		return ""
	}
	lines := make([]string, 0, 2)
	current := ""
	for _, hint := range hints {
		candidate := hint
		if current != "" {
			candidate = current + "  " + hint
		}
		if current != "" && lipgloss.Width(candidate) > width {
			lines = append(lines, current)
			current = hint
			continue
		}
		current = candidate
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderDashboard() string {
	content, _ := m.renderDashboardContent()
	return content
}

func (m Model) renderDashboardContent() (string, int) {
	if m.stats == nil && m.rules == nil && m.config == nil {
		if err := m.dataLoadError(); err != nil {
			return m.renderDaemonUnavailable(err), 0
		}
		return m.renderEmptyPanel(m.t("tui.state.connecting"), m.t("tui.dashboard.waiting_response"), false), 0
	}

	sections := make([]string, 0, 4)
	if m.height < 18 {
		sections = append(sections, m.renderDashboardMinimal())
	} else {
		layout := detectLayout(m.width)
		if layout <= layoutCompact || m.height < 28 {
			width := calcFullWidth(m.width)
			sections = append(sections, panelStyle.Width(width).Render(m.renderTrafficPanel()))
		} else {
			system := m.renderSystemPanel()
			traffic := m.renderTrafficPanel()
			if layout >= layoutMedium {
				leftWidth, rightWidth := calcRowWidths(m.width)
				sections = append(sections, lipgloss.JoinHorizontal(lipgloss.Top,
					panelStyle.Width(leftWidth).Render(system),
					panelStyle.Width(rightWidth).Render(traffic),
				))
			} else {
				width := calcFullWidth(m.width)
				sections = append(sections,
					panelStyle.Width(width).Render(system)+"\n"+
						panelStyle.Width(width).Render(traffic),
				)
			}
		}
	}

	networkOverview, networkInterfaces := m.renderNetworkSections()
	networkOffset := dashboardSectionLineOffset(sections)
	sections = append(sections, networkOverview)
	sections = append(sections, panelStyle.Width(calcFullWidth(m.width)).Render(m.renderTopRulesPanel()))
	if networkInterfaces != "" {
		sections = append(sections, networkInterfaces)
	}
	return strings.Join(sections, "\n"), networkOffset
}

func dashboardSectionLineOffset(sections []string) int {
	if len(sections) == 0 {
		return 0
	}
	return strings.Count(strings.Join(sections, "\n"), "\n") + 1
}

func (m Model) dashboardNetworkYOffset() int {
	content, offset := m.renderDashboardContent()
	limit := viewportMaxYOffset(content, m.mainViewportHeight())
	return max(0, min(offset, limit))
}

func (m Model) renderDaemonUnavailable(err error) string {
	panelWidth := calcFullWidth(m.width)
	innerWidth := panelInnerWidth(panelWidth)
	compact := m.width < 60 || m.height < 18
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.dashboard.daemon_unavailable")))
	if compact {
		b.WriteString("\n")
	} else {
		b.WriteString("\n\n")
	}
	b.WriteString(badgeOfflineStyle.Render(m.t("tui.state.offline")))
	b.WriteString("\n")
	message := m.friendlyError(err)
	if compact {
		message = truncate(message, innerWidth)
	}
	b.WriteString(subtleStyle.Render(ansi.Wrap(message, innerWidth, "")))
	if m.width >= 60 {
		b.WriteString("\n")
		b.WriteString(kvWrapped(m.t("tui.label.server"), m.client.BaseURL(), innerWidth))
	}
	detail, recovery := m.serviceRecoveryCopy(compact)
	if detail != "" {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(ansi.Wrap(safeText(detail), innerWidth, "")))
	}
	if recovery != "" {
		b.WriteString("\n")
		b.WriteString(valueStyle.Render(ansi.Wrap(safeText(recovery), innerWidth, "")))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderDashboardMinimal() string {
	var totalUpload, totalDownload, connections int64
	if m.stats != nil {
		for _, snapshot := range m.stats.Items {
			totalUpload += snapshot.UploadBytes
			totalDownload += snapshot.DownloadBytes
			connections += snapshot.Conns
		}
	}
	var uploadRate, downloadRate float64
	for _, rate := range m.rates {
		uploadRate += rate.UploadRate
		downloadRate += rate.DownloadRate
	}
	running := 0
	if m.rules != nil {
		running = len(m.rules.Items)
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.panel.overview")))
	b.WriteString("\n")
	b.WriteString(m.tf("tui.dashboard.upload", formatBytes(totalUpload), formatRate(uploadRate)))
	b.WriteString("\n")
	b.WriteString(m.tf("tui.dashboard.download", formatBytes(totalDownload), formatRate(downloadRate)))
	b.WriteString("\n")
	activity := m.tf("tui.dashboard.connections", connections) + "  " +
		m.tf("tui.dashboard.rules", running, len(m.displayRules()))
	b.WriteString(truncate(activity, panelInnerWidth(calcFullWidth(m.width))))
	if network := m.hostNetwork(); network != nil && network.Available {
		_, _, rxRate, txRate, counters := m.networkTotals(network)
		if counters > 0 {
			b.WriteString("\n")
			hostRates := fmt.Sprintf("%s %s  %s %s",
				m.t("tui.label.host_rx"), formatRate(rxRate),
				m.t("tui.label.host_tx"), formatRate(txRate))
			b.WriteString(truncate(hostRates, panelInnerWidth(calcFullWidth(m.width))))
		}
		if iface, address, ok := m.preferredNetworkAddress(network); ok {
			b.WriteString("\n")
			primary := iface.Name
			if address.CIDR != "" {
				primary += "  " + displayNetworkAddress(address)
			}
			b.WriteString(kv(m.t("tui.label.primary_network"), primary))
		}
	}
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderSystemPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("System", m.t("tui.panel.system")))
	b.WriteString("\n")
	status := m.t("tui.state.connecting_upper")
	if m.connectionErr != nil {
		status = m.t("tui.state.offline")
	} else if m.stats != nil {
		status = m.t("tui.state.online")
	}
	running := 0
	if m.rules != nil {
		running = len(m.rules.Items)
	}
	b.WriteString(kv(m.t("tui.label.status"), status))
	b.WriteString(kv(m.t("tui.label.running"), m.tf("tui.value.rules", running)))
	b.WriteString(kv(m.t("tui.label.configured"), m.tf("tui.value.rules", len(m.displayRules()))))
	if attention := m.ruleAttentionCount(); attention > 0 && (detectLayout(m.width) >= layoutMedium || m.height >= 32) {
		b.WriteString(kvStyled(m.t("tui.label.attention"), badgeOfflineStyle.Render(m.tf("tui.value.rules", attention))))
	}
	if network := m.hostNetwork(); network != nil && network.Available {
		valueWidth := m.systemPanelValueWidth()
		if network.Hostname != "" {
			b.WriteString(kv(m.t("tui.label.host"), truncate(network.Hostname, valueWidth)))
		}
		if iface, address, ok := m.preferredNetworkAddress(network); ok {
			primary := iface.Name
			if address.CIDR != "" {
				primary += "  " + displayNetworkAddress(address)
			}
			b.WriteString(kv(m.t("tui.label.primary_network"), truncate(primary, valueWidth)))
		}
	}
	b.WriteString(kv(m.t("tui.label.server"), m.client.BaseURL()))
	return b.String()
}

func (m Model) systemPanelValueWidth() int {
	panelWidth := calcFullWidth(m.width)
	if detectLayout(m.width) >= layoutMedium {
		panelWidth, _ = calcRowWidths(m.width)
	}
	return max(panelInnerWidth(panelWidth)-17, 1)
}

func (m Model) ruleAttentionCount() int {
	count := 0
	for _, rule := range m.displayRules() {
		state := dashboardStatePriority(m.ruleState(rule))
		if state <= 2 {
			count++
		}
	}
	return count
}

func (m Model) dataLoadError() error {
	if m.connectionErr != nil {
		return m.connectionErr
	}
	if m.configErr != nil {
		return m.configErr
	}
	return m.rulesErr
}

func (m Model) renderEmptyPanel(title, detail string, failed bool) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n\n")
	status := badgePausedStyle.Render(m.t("tui.state.waiting"))
	if failed {
		status = badgeOfflineStyle.Render(m.t("tui.state.offline"))
	}
	b.WriteString(status)
	if detail != "" {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(truncate(detail, panelInnerWidth(calcFullWidth(m.width)))))
	}
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderTrafficPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("Traffic", m.t("tui.panel.forwarding_traffic")))
	b.WriteString("\n")
	var totalUpload, totalDownload, connections int64
	if m.stats != nil {
		for _, snapshot := range m.stats.Items {
			totalUpload += snapshot.UploadBytes
			totalDownload += snapshot.DownloadBytes
			connections += snapshot.Conns
		}
	}
	b.WriteString(kv(m.t("tui.label.forwarded_upload"), formatBytes(totalUpload)))
	b.WriteString(kv(m.t("tui.label.forwarded_download"), formatBytes(totalDownload)))
	b.WriteString(kv(m.t("tui.label.active_connections"), fmt.Sprintf("%d", connections)))
	var uploadRate, downloadRate float64
	for _, rate := range m.rates {
		uploadRate += rate.UploadRate
		downloadRate += rate.DownloadRate
	}
	b.WriteString(kv(m.t("tui.label.upload_rate"), formatRate(uploadRate)))
	b.WriteString(kv(m.t("tui.label.download_rate"), formatRate(downloadRate)))
	return b.String()
}

func (m Model) renderNetworkSections() (string, string) {
	panelWidth := calcFullWidth(m.width)
	if m.stats == nil {
		detail := m.t("tui.network.waiting")
		if m.connectionErr != nil {
			detail = m.tf("tui.error.connection", m.friendlyError(m.connectionErr))
		}
		return m.renderNetworkUnavailable(panelWidth, detail), ""
	}
	network := m.stats.HostNetwork
	if network == nil {
		return m.renderNetworkUnavailable(panelWidth, m.t("tui.network.unsupported")), ""
	}
	if !network.Available {
		detail := m.t("tui.network.unavailable")
		if warnings := m.networkWarningText(network.Warnings); warnings != "" {
			detail += " " + warnings
		}
		return m.renderNetworkUnavailable(panelWidth, detail), ""
	}

	overview := panelStyle.Width(panelWidth).Render(m.renderNetworkOverview(network, panelWidth))
	interfaces := panelStyle.Width(panelWidth).Render(m.renderNetworkInterfaces(network, panelWidth))
	return overview, interfaces
}

func (m Model) renderNetworkUnavailable(panelWidth int, detail string) string {
	innerWidth := panelInnerWidth(panelWidth)
	var b strings.Builder
	b.WriteString(panelTitle("Network", m.t("tui.panel.host_network")))
	b.WriteString("\n\n")
	b.WriteString(badgePausedStyle.Render(m.t("tui.state.unavailable")))
	if detail != "" {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(ansi.Wrap(safeText(detail), innerWidth, "")))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderNetworkOverview(network *HostNetworkSnapshot, panelWidth int) string {
	innerWidth := panelInnerWidth(panelWidth)
	var b strings.Builder
	b.WriteString(panelTitle("Network", m.t("tui.panel.host_network")))
	b.WriteString("\n")
	hostname := strings.TrimSpace(network.Hostname)
	if hostname == "" {
		hostname = m.t("tui.value.unknown")
	}
	b.WriteString(kvWrapped(m.t("tui.label.host"), hostname, innerWidth))
	platform := strings.Trim(strings.TrimSpace(network.OS)+"/"+strings.TrimSpace(network.Arch), "/")
	if platform == "" {
		platform = m.t("tui.value.unknown")
	}
	b.WriteString(kvWrapped(m.t("tui.label.platform"), platform, innerWidth))
	up := 0
	for _, iface := range network.Interfaces {
		if iface.Up {
			up++
		}
	}
	b.WriteString(kvWrapped(m.t("tui.label.interfaces"), m.tf("tui.network.interface_count", up, len(network.Interfaces)), innerWidth))

	rxBytes, txBytes, rxRate, txRate, counters := m.networkTotals(network)
	if counters > 0 {
		b.WriteString(kvWrapped(m.t("tui.label.host_rx"), fmt.Sprintf("%s  %s", formatRate(rxRate), m.tf("tui.network.total", formatNetworkBytes(rxBytes))), innerWidth))
		b.WriteString(kvWrapped(m.t("tui.label.host_tx"), fmt.Sprintf("%s  %s", formatRate(txRate), m.tf("tui.network.total", formatNetworkBytes(txBytes))), innerWidth))
	} else {
		b.WriteString(kvWrapped(m.t("tui.label.host_traffic"), m.t("tui.network.counters_unavailable"), innerWidth))
	}
	if warnings := m.networkWarningText(network.Warnings); warnings != "" {
		b.WriteString(kvWrapped(m.t("tui.label.note"), warnings, innerWidth))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m Model) renderNetworkInterfaces(network *HostNetworkSnapshot, panelWidth int) string {
	innerWidth := panelInnerWidth(panelWidth)
	interfaces, hidden := m.visibleNetworkInterfaces(network.Interfaces)
	var b strings.Builder
	b.WriteString(panelTitle("Network", m.t("tui.network.interfaces")))
	b.WriteString("\n")
	if len(interfaces) == 0 {
		b.WriteString(subtleStyle.Render(m.t("tui.network.no_interfaces")))
		return b.String()
	}
	for index, iface := range interfaces {
		if index > 0 {
			b.WriteString(panelDivider(panelWidth, 0))
			b.WriteString("\n")
		}
		b.WriteString(m.renderNetworkInterface(iface, innerWidth))
	}
	if hidden > 0 {
		b.WriteString(panelDivider(panelWidth, 0))
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.tf("tui.network.hidden_interfaces", hidden)))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m Model) renderNetworkInterface(iface HostNetworkInterface, innerWidth int) string {
	state := m.t("tui.state.stopped")
	stateStyle := subtleStyle
	if iface.Up {
		state = m.t("tui.network.up")
		stateStyle = badgeOnlineStyle
	}
	attributes := []string{stateStyle.Render(state)}
	if iface.Loopback {
		attributes = append(attributes, subtleStyle.Render(m.t("tui.network.loopback")))
	} else if likelyVirtualInterface(iface.Name) {
		attributes = append(attributes, subtleStyle.Render(m.t("tui.network.virtual")))
	}
	if iface.MTU > 0 {
		attributes = append(attributes, subtleStyle.Render(fmt.Sprintf("MTU %d", iface.MTU)))
	}
	nameWidth := max(innerWidth-lipgloss.Width(strings.Join(attributes, "  "))-2, 8)
	header := valueStyle.Bold(true).Render(truncate(iface.Name, nameWidth)) + "  " + strings.Join(attributes, "  ")
	var b strings.Builder
	b.WriteString(ansi.Truncate(header, innerWidth, ""))
	b.WriteString("\n")
	if iface.CountersAvailable {
		rate := m.networkRates[iface.Name]
		traffic := fmt.Sprintf("RX %s (%s)  TX %s (%s)",
			formatRate(rate.DownloadRate), formatNetworkBytes(iface.RXBytes),
			formatRate(rate.UploadRate), formatNetworkBytes(iface.TXBytes))
		b.WriteString("  ")
		b.WriteString(ansi.Wrap(valueStyle.Render(traffic), max(innerWidth-2, 1), ""))
		b.WriteString("\n")
	}
	if len(iface.Addresses) == 0 {
		b.WriteString("  ")
		b.WriteString(subtleStyle.Render(m.t("tui.network.no_addresses")))
		b.WriteString("\n")
		return b.String()
	}
	for _, address := range iface.Addresses {
		line := fmt.Sprintf("%s  %s  %s", strings.ToUpper(address.Family), displayNetworkAddress(address), m.networkScopeLabel(address.Scope))
		b.WriteString("  ")
		b.WriteString(ansi.Wrap(safeText(line), max(innerWidth-2, 1), ""))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) visibleNetworkInterfaces(items []HostNetworkInterface) ([]HostNetworkInterface, int) {
	interfaces := append([]HostNetworkInterface(nil), items...)
	sort.SliceStable(interfaces, func(i, j int) bool {
		leftRate := m.networkRates[interfaces[i].Name]
		rightRate := m.networkRates[interfaces[j].Name]
		leftActive := leftRate.UploadRate > 0 || leftRate.DownloadRate > 0
		rightActive := rightRate.UploadRate > 0 || rightRate.DownloadRate > 0
		if leftActive != rightActive {
			return leftActive
		}
		if interfaces[i].Up != interfaces[j].Up {
			return interfaces[i].Up
		}
		if interfaces[i].Loopback != interfaces[j].Loopback {
			return !interfaces[i].Loopback
		}
		leftPriority := networkInterfacePriority(interfaces[i].Name)
		rightPriority := networkInterfacePriority(interfaces[j].Name)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return strings.ToLower(interfaces[i].Name) < strings.ToLower(interfaces[j].Name)
	})
	if m.networkExpanded {
		return interfaces, 0
	}
	visible := make([]HostNetworkInterface, 0, len(interfaces))
	hidden := 0
	for _, iface := range interfaces {
		rate := m.networkRates[iface.Name]
		active := rate.UploadRate > 0 || rate.DownloadRate > 0
		if iface.Loopback || !iface.Up || (likelyVirtualInterface(iface.Name) && !active) {
			hidden++
			continue
		}
		visible = append(visible, iface)
	}
	if len(visible) == 0 && len(interfaces) > 0 {
		visible = append(visible, interfaces[0])
		hidden = len(interfaces) - 1
	}
	return visible, hidden
}

func (m Model) networkTotals(network *HostNetworkSnapshot) (rxBytes, txBytes uint64, rxRate, txRate float64, counters int) {
	if network == nil {
		return 0, 0, 0, 0, 0
	}
	for _, iface := range network.Interfaces {
		if iface.Loopback || !iface.CountersAvailable {
			continue
		}
		rxBytes += iface.RXBytes
		txBytes += iface.TXBytes
		rate := m.networkRates[iface.Name]
		rxRate += rate.DownloadRate
		txRate += rate.UploadRate
		counters++
	}
	return rxBytes, txBytes, rxRate, txRate, counters
}

func (m Model) hostNetwork() *HostNetworkSnapshot {
	if m.stats == nil {
		return nil
	}
	return m.stats.HostNetwork
}

func (m Model) preferredNetworkAddress(network *HostNetworkSnapshot) (HostNetworkInterface, HostNetworkAddress, bool) {
	if network == nil {
		return HostNetworkInterface{}, HostNetworkAddress{}, false
	}
	interfaces := append([]HostNetworkInterface(nil), network.Interfaces...)
	sort.SliceStable(interfaces, func(i, j int) bool {
		left, right := interfaces[i], interfaces[j]
		if left.Up != right.Up {
			return left.Up
		}
		if left.Loopback != right.Loopback {
			return !left.Loopback
		}
		leftVirtual, rightVirtual := likelyVirtualInterface(left.Name), likelyVirtualInterface(right.Name)
		if leftVirtual != rightVirtual {
			return !leftVirtual
		}
		leftPriority, rightPriority := networkInterfacePriority(left.Name), networkInterfacePriority(right.Name)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if left.Index > 0 && right.Index > 0 && left.Index != right.Index {
			return left.Index < right.Index
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
	for _, iface := range interfaces {
		if !iface.Up || iface.Loopback {
			continue
		}
		for _, address := range iface.Addresses {
			if address.Family == "ipv4" && (address.Scope == "global_unicast" || address.Scope == "private") {
				return iface, address, true
			}
		}
		for _, address := range iface.Addresses {
			if address.Scope == "global_unicast" || address.Scope == "private" {
				return iface, address, true
			}
		}
	}
	return HostNetworkInterface{}, HostNetworkAddress{}, false
}

func (m Model) networkWarningText(warnings []string) string {
	labels := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		key := "tui.network.warning." + warning
		label := m.t(key)
		if label != key {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, "; ")
}

func (m Model) networkScopeLabel(scope string) string {
	key := "tui.network.scope." + strings.TrimSpace(scope)
	label := m.t(key)
	if label == key {
		return scope
	}
	return label
}

func displayNetworkAddress(address HostNetworkAddress) string {
	if address.Zone == "" {
		return address.CIDR
	}
	host, prefix, ok := strings.Cut(address.CIDR, "/")
	if !ok {
		return host + "%" + address.Zone
	}
	return host + "%" + address.Zone + "/" + prefix
}

func likelyVirtualInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	prefixes := []string{"br-", "bridge", "cni", "docker", "dummy", "flannel", "podman", "tap", "tailscale", "tun", "utun", "veth", "virbr", "vethernet", "wg"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func networkInterfacePriority(name string) int {
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasPrefix(name, "eth"), strings.HasPrefix(name, "en"):
		return 0
	case strings.HasPrefix(name, "wl"), strings.HasPrefix(name, "wi-fi"), strings.HasPrefix(name, "ethernet"):
		return 1
	case likelyVirtualInterface(name):
		return 3
	default:
		return 2
	}
}

func (m Model) renderTopRulesPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("Rules", m.t("tui.panel.rules")))
	b.WriteString("\n")
	rules := m.dashboardRules()
	if len(rules) == 0 {
		message := m.t("tui.rules.none")
		if m.config == nil && m.rules == nil {
			if m.dataLoadError() != nil {
				message = m.t("tui.rules.unavailable")
			} else {
				message = m.t("tui.rules.loading")
			}
		}
		b.WriteString(subtleStyle.Render(message))
		return b.String()
	}
	stats := m.statsMap()
	limit := min(len(rules), 5)
	for index := 0; index < limit; index++ {
		rule := rules[index]
		snapshot := stats[rule.RuleID]
		rate := m.rates[rule.RuleID]
		var line string
		switch {
		case m.width < 50:
			line = fmt.Sprintf("%s %s %s %3d",
				cellColumn(rule.Name, 10), cellColumn(m.ruleState(rule), 8), protocolLabel(string(rule.Protocol)), snapshot.Conns)
		case m.width < 70:
			line = fmt.Sprintf("%s %s %s %4d U%7s D%7s",
				cellColumn(rule.Name, 14), cellColumn(m.ruleState(rule), 8), protocolLabel(string(rule.Protocol)), snapshot.Conns,
				formatRate(rate.UploadRate), formatRate(rate.DownloadRate))
		default:
			line = m.tf("tui.dashboard.rule_rates",
				cellColumn(rule.Name, 14), cellColumn(m.ruleState(rule), 8), protocolLabel(string(rule.Protocol)), snapshot.Conns,
				formatRate(rate.UploadRate), formatRate(rate.DownloadRate))
		}
		b.WriteString(valueStyle.Render(line))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) dashboardRules() []RuleInfo {
	rules := m.displayRules()
	stats := m.statsMap()
	sort.SliceStable(rules, func(i, j int) bool {
		leftState := dashboardStatePriority(m.ruleState(rules[i]))
		rightState := dashboardStatePriority(m.ruleState(rules[j]))
		if leftState != rightState {
			return leftState < rightState
		}
		leftRate := m.rates[rules[i].RuleID]
		rightRate := m.rates[rules[j].RuleID]
		leftTotal := leftRate.UploadRate + leftRate.DownloadRate
		rightTotal := rightRate.UploadRate + rightRate.DownloadRate
		if leftTotal != rightTotal {
			return leftTotal > rightTotal
		}
		if stats[rules[i].RuleID].Conns != stats[rules[j].RuleID].Conns {
			return stats[rules[i].RuleID].Conns > stats[rules[j].RuleID].Conns
		}
		return strings.ToLower(rules[i].Name) < strings.ToLower(rules[j].Name)
	})
	return rules
}

func dashboardStatePriority(state string) int {
	state = strings.ToLower(strings.TrimSpace(state))
	switch {
	case strings.Contains(state, "error"), strings.Contains(state, "错误"):
		return 0
	case strings.Contains(state, "drift"), strings.Contains(state, "偏差"):
		return 1
	case strings.Contains(state, "pending"), strings.Contains(state, "待处理"):
		return 2
	case strings.Contains(state, "running"), strings.Contains(state, "运行"):
		return 3
	default:
		return 4
	}
}

func (m Model) renderRules() string {
	rules := m.sortedRules()
	allRules := m.displayRules()
	filterValue := strings.TrimSpace(m.filterInput.Value())
	fullWidth := calcFullWidth(m.width)
	var b strings.Builder
	if m.filterActive {
		b.WriteString(responsiveTextInputView(m.filterInput, panelInnerWidth(fullWidth), panelInnerWidth(fullWidth)))
		b.WriteString("\n")
	} else if filterValue != "" {
		b.WriteString(subtleStyle.Render(m.tf("tui.rules.filter", truncate(filterValue, panelInnerWidth(fullWidth)-8))))
		b.WriteString("\n")
	}
	if m.rulesUnavailable() {
		return m.renderRulesUnavailable(fullWidth)
	}
	if m.config == nil && m.rules == nil {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.t("tui.rules.loading_sentence")))
		return panelStyle.Width(fullWidth).Render(b.String())
	}
	udpLimit := 0
	if m.draft != nil {
		udpLimit = m.draft.UDPMaxSessions
	} else if m.config != nil {
		udpLimit = m.config.UDPMaxSessions
	}
	if m.width < 50 {
		if filterValue != "" {
			b.WriteString(subtleStyle.Render(m.tf("tui.rules.summary.filtered_compact", len(rules), len(allRules), udpLimit)))
		} else {
			b.WriteString(subtleStyle.Render(m.tf("tui.rules.summary.compact", len(allRules), udpLimit)))
		}
	} else if filterValue != "" {
		b.WriteString(subtleStyle.Render(m.tf("tui.rules.summary.filtered", len(rules), len(allRules), udpLimit)))
	} else {
		b.WriteString(subtleStyle.Render(m.tf("tui.rules.summary", len(allRules), udpLimit)))
	}
	b.WriteString("\n")
	if notice := m.rulesAccessNotice(); notice != "" {
		b.WriteString(subtleStyle.Render("! " + truncate(notice, panelInnerWidth(fullWidth)-2)))
		b.WriteString("\n")
	}
	if workflow := m.renderRulesWorkflow(); workflow != "" {
		b.WriteString(workflow)
		b.WriteString("\n")
	}
	if len(rules) == 0 {
		b.WriteString("\n")
		switch {
		case filterValue != "" && len(allRules) > 0:
			b.WriteString(subtleStyle.Render(m.tf("tui.rules.no_matches", strconv.Quote(truncate(filterValue, 32)))))
		case m.config == nil && m.rules == nil && m.dataLoadError() != nil:
			b.WriteString(badgeOfflineStyle.Render(m.t("tui.rules.unavailable_sentence")))
			b.WriteString("\n")
			b.WriteString(subtleStyle.Render(m.friendlyError(m.dataLoadError())))
		case m.config == nil && m.rules == nil:
			b.WriteString(subtleStyle.Render(m.t("tui.rules.loading_sentence")))
		default:
			b.WriteString(subtleStyle.Render(m.t("tui.rules.none_sentence")))
			b.WriteString("\n")
			if m.canWrite() {
				b.WriteString(subtleStyle.Render(m.t("tui.rules.create_hint")))
			} else {
				b.WriteString(subtleStyle.Render(m.t("tui.rules.read_only_hint")))
			}
		}
		return panelStyle.Width(fullWidth).Render(b.String())
	}

	layout := detectLayout(m.width)
	switch {
	case m.width < 50:
		b.WriteString(strings.Join([]string{cellColumn(m.t("tui.column.name"), 12), cellColumn(m.t("tui.column.state"), 8), cellColumn(m.t("tui.column.protocol"), 7)}, " ") + "\n")
	case layout >= layoutWide:
		b.WriteString(strings.Join([]string{cellColumn(m.t("tui.column.name"), 16), cellColumn(m.t("tui.column.state"), 9), cellColumn(m.t("tui.column.change"), 9), cellColumn(m.t("tui.column.protocol"), 7), cellColumn(m.t("tui.column.listen"), 20), cellColumn(m.t("tui.column.target"), 20), cellColumn(m.t("tui.column.access"), 10), cellColumn(m.t("tui.column.connections"), 6), cellColumn(m.t("tui.column.upload_rate"), 9), cellColumn(m.t("tui.column.download_rate"), 9)}, " ") + "\n")
	case layout >= layoutMedium:
		b.WriteString(strings.Join([]string{cellColumn(m.t("tui.column.name"), 16), cellColumn(m.t("tui.column.state"), 9), cellColumn(m.t("tui.column.change"), 9), cellColumn(m.t("tui.column.protocol"), 7), cellColumn(m.t("tui.column.listen"), 18), cellColumn(m.t("tui.column.target"), 18), cellColumn(m.t("tui.column.connections"), 6)}, " ") + "\n")
	case layout >= layoutNarrow:
		b.WriteString(strings.Join([]string{cellColumn(m.t("tui.column.name"), 15), cellColumn(m.t("tui.column.state"), 9), cellColumn(m.t("tui.column.change"), 9), cellColumn(m.t("tui.column.protocol"), 7), cellColumn(m.t("tui.column.listen"), 17), cellColumn(m.t("tui.column.connections"), 6)}, " ") + "\n")
	default:
		b.WriteString(strings.Join([]string{cellColumn(m.t("tui.column.name"), 12), cellColumn(m.t("tui.column.state"), 8), cellColumn(m.t("tui.column.change"), 7), cellColumn(m.t("tui.column.protocol"), 7)}, " ") + "\n")
	}
	b.WriteString(panelDivider(fullWidth, 0))
	b.WriteString("\n")

	start, end := m.visibleRuleRange(rules)
	hasRange := m.height >= 16 && (start > 0 || end < len(rules))
	stats := m.statsMap()
	for index := start; index < end; index++ {
		rule := rules[index]
		snapshot := stats[rule.RuleID]
		rate := m.rates[rule.RuleID]
		state := m.ruleState(rule)
		change := m.ruleChange(rule.RuleID)
		listen := formatEndpoint(rule.ListenAddr, rule.ListenPort)
		target := formatEndpoint(rule.TargetAddr, rule.TargetPort)
		var line string
		switch {
		case m.width < 50:
			marker := " "
			switch {
			case strings.HasPrefix(change, "+"):
				marker = "+"
			case strings.HasPrefix(change, "-"):
				marker = "-"
			case strings.HasPrefix(change, "~"):
				marker = "~"
			case change != "":
				marker = "*"
			}
			name := marker + " " + cellColumn(rule.Name, 10)
			line = fmt.Sprintf("%s %s %s", name, cellColumn(state, 8), protocolLabel(string(rule.Protocol)))
		case layout >= layoutWide:
			line = fmt.Sprintf("%s %s %s %s %s %s %s %6d %9s %9s",
				cellColumn(rule.Name, 16), cellColumn(state, 9), cellColumn(change, 9), protocolLabel(string(rule.Protocol)),
				cellColumn(listen, 20), cellColumn(target, 20), cellColumn(sourceIPPolicySummaryWithTranslator(rule, m.tr), 10), snapshot.Conns,
				formatRate(rate.UploadRate), formatRate(rate.DownloadRate))
		case layout >= layoutMedium:
			line = fmt.Sprintf("%s %s %s %s %s %s %6d",
				cellColumn(rule.Name, 16), cellColumn(state, 9), cellColumn(change, 9), protocolLabel(string(rule.Protocol)),
				cellColumn(listen, 18), cellColumn(target, 18), snapshot.Conns)
		case layout >= layoutNarrow:
			line = fmt.Sprintf("%s %s %s %s %s %6d",
				cellColumn(rule.Name, 15), cellColumn(state, 9), cellColumn(change, 9), protocolLabel(string(rule.Protocol)),
				cellColumn(listen, 17), snapshot.Conns)
		default:
			line = fmt.Sprintf("%s %s %s %s",
				cellColumn(rule.Name, 12), cellColumn(state, 8), cellColumn(change, 7), protocolLabel(string(rule.Protocol)))
		}
		if rule.RuleID == m.selectedRuleID || (m.selectedRuleID == "" && index == 0) {
			line = selectedStyle.Render("> " + line)
		} else if m.isDeleted(rule.RuleID) {
			line = lipgloss.NewStyle().Foreground(CRed).Render("  " + line)
		} else {
			background := CRowA
			if index%2 == 1 {
				background = CRowB
			}
			line = lipgloss.NewStyle().Background(background).Render("  " + line)
		}
		b.WriteString(line)
		if index < end-1 || hasRange {
			b.WriteString("\n")
		}
	}
	if hasRange {
		b.WriteString(subtleStyle.Render(m.tf("tui.rules.rows", start+1, end, len(rules))))
	}
	return panelStyle.Width(fullWidth).Render(b.String())
}

func (m Model) rulesAccessNotice() string {
	if m.canWrite() || m.height < 16 || (m.session == nil && m.sessionErr == nil) {
		return ""
	}
	return m.writeDeniedReason()
}

func (m Model) renderRulesWorkflow() string {
	if m.draft == nil {
		return ""
	}
	changes := m.dirtyChangeCount()
	switch m.sync {
	case syncValidated:
		return syncStyle(syncValidated).Render(m.tf("tui.workflow.validated", changes))
	case syncStale:
		return syncStyle(syncStale).Render(m.tf("tui.workflow.stale", changes))
	default:
		return syncStyle(syncDirty).Render(m.tf("tui.workflow.draft", changes))
	}
}

func (m Model) rulesUnavailable() bool {
	return m.config == nil && m.rules == nil && m.dataLoadError() != nil
}

func (m Model) renderRulesUnavailable(fullWidth int) string {
	innerWidth := panelInnerWidth(fullWidth)
	var b strings.Builder
	b.WriteString(badgeOfflineStyle.Render(m.t("tui.rules.status_unknown")))
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(ansi.Wrap(m.friendlyError(m.dataLoadError()), innerWidth, "")))
	if m.width >= 60 {
		b.WriteString("\n")
		b.WriteString(kvWrapped(m.t("tui.label.server"), m.client.BaseURL(), innerWidth))
	}
	b.WriteString("\n")
	b.WriteString(valueStyle.Render(ansi.Wrap(m.t("tui.rules.recovery"), innerWidth, "")))
	return panelStyle.Width(fullWidth).Render(b.String())
}

func (m Model) visibleRuleRange(rules []RuleInfo) (int, int) {
	maxRows := m.rulePageSize()
	if len(rules) <= maxRows {
		return 0, len(rules)
	}
	selected := m.selectedIndex()
	start := selected - maxRows/2
	start = max(0, min(start, len(rules)-maxRows))
	return start, start + maxRows
}

func (m Model) rulePageSize() int {
	maxRows := max(m.height-11, 3)
	if m.draft != nil {
		maxRows = max(maxRows-1, 1)
	}
	if m.rulesAccessNotice() != "" {
		maxRows = max(maxRows-1, 1)
	}
	if m.height < 16 && strings.TrimSpace(m.filterInput.Value()) != "" {
		return 2
	}
	return maxRows
}

func (m Model) renderDetail() string {
	rule := m.selectedRule()
	if rule == nil {
		return subtleStyle.Render(m.t("tui.detail.no_selection"))
	}
	panelWidth := calcFullWidth(m.width)
	var b strings.Builder
	b.WriteString(titleStyle.Render(truncate(rule.Name, max(panelInnerWidth(panelWidth)-24, 8))))
	b.WriteString("  ")
	b.WriteString(subtleStyle.Render(m.ruleState(*rule)))
	if change := m.ruleChange(rule.RuleID); change != "" {
		b.WriteString("  ")
		b.WriteString(syncStyle(syncDirty).Render(change))
	}
	b.WriteString("\n")
	b.WriteString(panelDivider(panelWidth, 70))
	b.WriteString("\n")
	innerWidth := panelInnerWidth(panelWidth)
	b.WriteString(kvWrapped(m.t("tui.editor.field.rule_id"), rule.RuleID, innerWidth))
	b.WriteString(kvWrapped(m.t("tui.editor.field.protocol"), string(rule.Protocol), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.column.listen"), formatEndpoint(rule.ListenAddr, rule.ListenPort), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.column.target"), formatEndpoint(rule.TargetAddr, rule.TargetPort), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.editor.field.enabled"), boolStrWithTranslator(rule.Enabled, m.tr), innerWidth))
	if rule.Bandwidth != nil {
		b.WriteString(kvWrapped(m.t("tui.detail.bandwidth_upload"), bandwidthLimitValue(rule.Bandwidth.UploadBytesPerSecond(), m.tr), innerWidth))
		b.WriteString(kvWrapped(m.t("tui.detail.bandwidth_download"), bandwidthLimitValue(rule.Bandwidth.DownloadBytesPerSecond(), m.tr), innerWidth))
	}
	b.WriteString(kvWrapped(m.t("tui.detail.speed_limit"), speedLimitStrWithTranslator(rule.SpeedLimit, m.tr), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.detail.max_connections"), maxConnStrWithTranslator(rule.MaxConn, m.tr), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.detail.idle_timeout"), fmt.Sprintf("%ds", rule.IdleTimeout), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.detail.ip_access"), sourceIPModeLabelWithTranslator(rule.SourceIPMode, m.tr), innerWidth))
	if len(rule.SourceIPs) > 0 {
		b.WriteString(kvWrapped(m.t("tui.detail.source_ips"), strings.Join(rule.SourceIPs, ", "), innerWidth))
	}
	if rule.Remark != "" {
		b.WriteString(kvWrapped(m.t("tui.editor.field.remark"), rule.Remark, innerWidth))
	}
	stats := m.statsMap()[rule.RuleID]
	rate := m.rates[rule.RuleID]
	b.WriteString("\n")
	b.WriteString(panelTitle("Traffic", m.t("tui.panel.traffic")))
	b.WriteString("\n")
	b.WriteString(kvWrapped(m.t("tui.label.upload"), fmt.Sprintf("%s (%s)", formatBytes(stats.UploadBytes), formatRate(rate.UploadRate)), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.label.download"), fmt.Sprintf("%s (%s)", formatBytes(stats.DownloadBytes), formatRate(rate.DownloadRate)), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.label.connections"), fmt.Sprintf("%d", stats.Conns), innerWidth))
	b.WriteString(kvWrapped(m.t("tui.detail.ip_denied"), fmt.Sprintf("%d", stats.SourceIPDenied), innerWidth))
	if rule.TrafficQuota != nil {
		quotaStatus := m.t("tui.value.unknown")
		if stats.Quota != nil {
			quotaStatus = m.t("tui.value.active")
			if stats.Quota.Blocked {
				quotaStatus = m.t("tui.value.blocked")
			}
		}
		b.WriteString(kvWrapped(m.t("tui.detail.quota_status"), quotaStatus, innerWidth))
		if stats.Quota == nil {
			if rule.TrafficQuota.Daily > 0 {
				b.WriteString(kvWrapped(m.t("tui.detail.quota_daily"), m.t("tui.value.unknown"), innerWidth))
			}
			if rule.TrafficQuota.Monthly > 0 {
				b.WriteString(kvWrapped(m.t("tui.detail.quota_monthly"), m.t("tui.value.unknown"), innerWidth))
			}
		} else {
			if stats.Quota.Daily != nil {
				b.WriteString(kvWrapped(m.t("tui.detail.quota_daily"), formatQuotaPeriod(stats.Quota.Daily, rule.TrafficQuota.Timezone, m.tr), innerWidth))
			}
			if stats.Quota.Monthly != nil {
				b.WriteString(kvWrapped(m.t("tui.detail.quota_monthly"), formatQuotaPeriod(stats.Quota.Monthly, rule.TrafficQuota.Timezone, m.tr), innerWidth))
			}
		}
		b.WriteString(kvWrapped(m.t("tui.detail.quota_rejected"), formatBytes(stats.QuotaRejectedBytes), innerWidth))
	}
	if history := m.history[rule.RuleID]; history != nil && len(history.uploadRates) > 1 && m.height >= 24 && historyHasTraffic(history) {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.t("tui.detail.upload_rate")))
		b.WriteString("\n")
		b.WriteString(renderSparkline(history.uploadRates, CUpload, calcFullWidth(m.width)-6))
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.t("tui.detail.download_rate")))
		b.WriteString("\n")
		b.WriteString(renderSparkline(history.downloadRates, CDownload, calcFullWidth(m.width)-6))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func bandwidthLimitValue(bytesPerSecond int64, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	if bytesPerSecond <= 0 {
		return translator.T("tui.value.unlimited")
	}
	return engine.ByteRate(bytesPerSecond).String()
}

func formatQuotaPeriod(period *engine.QuotaPeriodSnapshot, timezone string, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	if period == nil {
		return translator.T("tui.value.unknown")
	}
	location, err := time.LoadLocation(strings.TrimSpace(timezone))
	if err != nil {
		location = time.UTC
	}
	reset := time.Unix(period.ResetTime, 0).In(location).Format("2006-01-02 15:04 MST")
	status := translator.T("tui.value.active")
	if period.Exhausted {
		status = translator.T("tui.value.exhausted")
	}
	return translator.Tf(
		"tui.detail.quota_period",
		formatBytes(period.UsedBytes),
		engine.ByteSize(period.LimitBytes).String(),
		formatBytes(period.RemainingBytes),
		reset,
		status,
	)
}

func historyHasTraffic(history *rateHistory) bool {
	if history == nil {
		return false
	}
	for _, rates := range [][]float64{history.uploadRates, history.downloadRates} {
		for _, rate := range rates {
			if rate > 0 {
				return true
			}
		}
	}
	return false
}

func (m Model) renderEditor() string {
	if m.editor == nil {
		return subtleStyle.Render(m.t("tui.editor.unavailable"))
	}
	panelWidth := calcFullWidth(m.width)
	inputWidth := max(panelInnerWidth(panelWidth)-23, 1)
	mode := m.t("tui.editor.mode.basic")
	if m.editor.advanced {
		mode = m.t("tui.editor.mode.advanced")
	}
	lines := []string{
		titleStyle.Render(m.editor.title()) + "  " + subtleStyle.Render(mode),
		panelDivider(panelWidth, 70),
	}
	indices := m.visibleEditorIndices()
	allVisible := m.editor.visibleFieldIndices()
	hasRange := len(indices) < len(allVisible)
	formError := m.editorFormErrorView()
	hasFormError := formError != ""
	for _, index := range indices {
		field := m.editor.fields[index]
		prefix := "  "
		if index == m.editor.focus {
			prefix = "> "
		}
		var value string
		if message := m.editor.errors[field.key]; message != "" {
			value = badgeOfflineStyle.Render(truncate(message, inputWidth))
		} else {
			switch field.kind {
			case editorProtocol:
				value = "< " + strings.ToUpper(field.choice) + " >"
			case editorSourceIPMode:
				value = "< " + sourceIPModeLabelWithTranslator(engine.SourceIPMode(field.choice), m.tr) + " >"
			case editorToggle:
				if field.enabled {
					value = m.t("tui.editor.enabled")
				} else {
					value = m.t("tui.editor.disabled")
				}
			case editorReadOnly:
				value = subtleStyle.Render(truncate(m.tf("tui.editor.fixed", field.input.Value()), inputWidth))
			default:
				if index == m.editor.focus {
					value = field.inputView(inputWidth)
				} else {
					value = truncate(field.input.Value(), inputWidth)
				}
			}
		}
		lines = append(lines, prefix+cellColumn(field.label+":", 20)+" "+value)
	}
	if hasRange {
		first := m.editorVisiblePosition(indices[0], allVisible) + 1
		last := m.editorVisiblePosition(indices[len(indices)-1], allVisible) + 1
		lines = append(lines, subtleStyle.Render(m.tf("tui.editor.fields", first, last, len(allVisible))))
	}
	if hasFormError {
		lines = append(lines, badgeOfflineStyle.Render(formError))
	}
	if m.height >= 18 {
		if hint := m.editorFieldHint(); hint != "" {
			lines = append(lines, subtleStyle.Render("? "+truncate(hint, panelInnerWidth(panelWidth)-2)))
		}
	}
	if m.height >= 22 {
		if warning := m.editorExposureWarning(); warning != "" {
			lines = append(lines, badgePausedStyle.Render("! "+truncate(warning, panelInnerWidth(panelWidth)-2)))
		}
	}
	return panelStyle.Width(panelWidth).Render(strings.Join(lines, "\n"))
}

func (m Model) visibleEditorIndices() []int {
	if m.editor == nil {
		return nil
	}
	indices := m.editor.visibleFieldIndices()
	if len(indices) == 0 {
		return nil
	}
	available := m.mainViewportHeight()
	if formError := m.editorFormErrorView(); formError != "" {
		available -= lipgloss.Height(formError)
	}
	if m.height >= 18 {
		available--
	}
	if m.height >= 22 && m.editorExposureWarning() != "" {
		available--
	}
	maxRows := max(available-4, 1)
	if len(indices) > maxRows {
		maxRows = max(available-5, 1)
	}
	if len(indices) <= maxRows {
		return indices
	}
	focus := m.editorVisiblePosition(m.editor.focus, indices)
	start := focus - maxRows/2
	start = max(0, min(start, len(indices)-maxRows))
	return indices[start : start+maxRows]
}

func (m Model) editorVisiblePosition(fieldIndex int, indices []int) int {
	for position, index := range indices {
		if index == fieldIndex {
			return position
		}
	}
	return 0
}

func (m Model) editorFieldHint() string {
	if m.editor == nil || m.editor.focus < 0 || m.editor.focus >= len(m.editor.fields) {
		return ""
	}
	key := "tui.editor.hint." + m.editor.fields[m.editor.focus].key
	value := m.t(key)
	if value == key {
		return ""
	}
	return value
}

func (m Model) editorExposureWarning() string {
	if m.editor == nil || m.editor.value(fieldEnabled) != "true" {
		return ""
	}
	listenAddr := strings.TrimSpace(m.editor.value(fieldListenAddr))
	mode := strings.ToLower(strings.TrimSpace(m.editor.value(fieldSourceIPMode)))
	if (listenAddr == "" || listenAddr == "0.0.0.0" || listenAddr == "::") && (mode == "" || mode == "off") {
		return m.t("tui.editor.exposure_warning")
	}
	return ""
}

func (m Model) editorFormErrorView() string {
	if m.editor == nil || m.editor.errors["form"] == "" {
		return ""
	}
	width := max(panelInnerWidth(calcFullWidth(m.width)), 1)
	return ansi.Wrap(safeText(m.editor.errors["form"]), width, "")
}

func (m Model) renderPrecheck() string {
	if m.precheckResult == nil {
		return subtleStyle.Render(m.t("tui.precheck.none"))
	}
	if m.width < 70 || m.height < 18 {
		return m.renderPrecheckCompact()
	}
	full := m.renderPrecheckFull()
	if viewportMaxYOffset(full, m.mainViewportHeight()) > 0 {
		return m.renderPrecheckCompact()
	}
	return full
}

func (m Model) renderPrecheckFull() string {
	result := m.precheckResult.Precheck
	var b strings.Builder
	status := badgeOfflineStyle.Render(m.t("tui.state.failed"))
	if result.OK {
		status = badgeOnlineStyle.Render(m.t("tui.state.passed"))
	}
	b.WriteString(titleStyle.Render(m.t("tui.precheck.title")))
	b.WriteString("  ")
	b.WriteString(status)
	b.WriteString("\n")
	b.WriteString(m.tf("tui.precheck.summary", result.CheckedRules, result.ErrorCount, result.WarningCount, result.CheckedTimeMS) + "\n")
	if result.OK {
		b.WriteString(syncStyle(syncValidated).Render(m.t("tui.precheck.ready_to_apply")) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(panelTitle("", m.t("tui.panel.changes")))
	b.WriteString("\n")
	changes := m.precheckDiffValues(m.precheckResult.Diff)
	if len(changes) == 0 {
		changes = m.sortedDiffValues(m.localDiff())
	}
	if len(changes) == 0 && m.draft != nil && m.config != nil && m.draft.UDPMaxSessions == m.config.UDPMaxSessions {
		b.WriteString(subtleStyle.Render(m.t("tui.precheck.no_changes")))
		b.WriteString("\n")
	} else {
		changeLimit := min(len(changes), max(m.height/4, 3))
		for _, line := range changes[:changeLimit] {
			b.WriteString("  " + safeText(line) + "\n")
		}
		if changeLimit < len(changes) {
			b.WriteString(subtleStyle.Render(m.tf("tui.precheck.more_changes", len(changes)-changeLimit)))
			b.WriteString("\n")
		}
	}
	if m.draft != nil && m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		b.WriteString(fmt.Sprintf("  ~ udp_max_sessions %d -> %d\n", m.config.UDPMaxSessions, m.draft.UDPMaxSessions))
	}

	b.WriteString("\n")
	b.WriteString(panelTitle("", m.t("tui.panel.findings")))
	b.WriteString("\n")
	if len(result.Items) == 0 {
		b.WriteString(badgeOnlineStyle.Render(m.t("tui.precheck.no_findings")))
		b.WriteString("\n")
	} else {
		b.WriteString(subtleStyle.Render("  " + strings.Join([]string{cellColumn(m.t("tui.column.severity"), 7), cellColumn(m.t("tui.column.rule"), 18), cellColumn(m.t("tui.column.check"), 20), m.t("tui.column.message")}, " ") + "\n"))
		start, end := m.visibleFindingRange(len(result.Items))
		for index := start; index < end; index++ {
			item := result.Items[index]
			line := strings.Join([]string{
				cellColumn(m.localizedSeverity(string(item.Severity)), 7),
				cellColumn(item.RuleID, 18),
				cellColumn(m.localizedPrecheckCheck(item.Check), 20),
				m.localizedPrecheckMessage(item),
			}, " ")
			if index == m.resultSelected {
				line = selectedStyle.Render("> " + truncate(line, max(m.width-8, 20)))
			} else if string(item.Severity) == "error" {
				line = badgeOfflineStyle.Render("  " + truncate(line, max(m.width-8, 20)))
			} else {
				line = badgePausedStyle.Render("  " + truncate(line, max(m.width-8, 20)))
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderPrecheckCompact() string {
	result := m.precheckResult.Precheck
	status := badgeOfflineStyle.Render(m.t("tui.state.failed"))
	if result.OK {
		status = badgeOnlineStyle.Render(m.t("tui.state.passed"))
	}
	changes := m.precheckDiffValues(m.precheckResult.Diff)
	if len(changes) == 0 {
		changes = m.sortedDiffValues(m.localDiff())
	}
	changeCount := len(changes)
	if m.draft != nil && m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		changeCount++
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.precheck.title")))
	b.WriteString("  ")
	b.WriteString(status)
	b.WriteString("\n")
	b.WriteString(m.tf("tui.precheck.summary_compact", result.CheckedRules, changeCount, result.ErrorCount, result.WarningCount) + "\n")
	if result.OK {
		b.WriteString(syncStyle(syncValidated).Render(m.t("tui.precheck.ready_to_apply")) + "\n")
	}
	b.WriteString(panelTitle("", m.t("tui.panel.findings")))
	if len(result.Items) == 0 {
		b.WriteString("\n")
		b.WriteString(badgeOnlineStyle.Render(m.t("tui.precheck.no_findings")))
		return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
	}
	b.WriteString("\n")
	maxRows := m.precheckPageSize()
	start := max(0, min(m.resultSelected-maxRows/2, len(result.Items)-maxRows))
	end := min(start+maxRows, len(result.Items))
	lineWidth := max(panelInnerWidth(calcFullWidth(m.width))-2, 1)
	for index := start; index < end; index++ {
		item := result.Items[index]
		severity := m.localizedSeverityShort(string(item.Severity))
		line := truncate(fmt.Sprintf("%s %s %s: %s", severity, item.RuleID, m.localizedPrecheckCheck(item.Check), m.localizedPrecheckMessage(item)), lineWidth)
		prefix := "  "
		style := badgePausedStyle
		if index == m.resultSelected {
			prefix = "> "
			style = selectedStyle
		} else if string(item.Severity) == "error" {
			style = badgeOfflineStyle
		}
		b.WriteString(style.Render(prefix + line))
		if index < end-1 {
			b.WriteString("\n")
		}
	}
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) visibleFindingRange(count int) (int, int) {
	maxRows := m.precheckPageSize()
	if count <= maxRows {
		return 0, count
	}
	start := m.resultSelected - maxRows/2
	start = max(0, min(start, count-maxRows))
	return start, start + maxRows
}

func (m Model) precheckPageSize() int {
	if m.width < 70 || m.height < 18 {
		extra := 5
		if m.precheckResult != nil && m.precheckResult.Precheck.OK {
			extra++
		}
		return max(m.mainViewportHeight()-extra, 1)
	}
	return max(m.height-17, 3)
}

func (m Model) renderApplyResult() string {
	if m.applyResult == nil {
		return subtleStyle.Render(m.t("tui.apply.none"))
	}
	if m.height < 18 {
		return m.renderApplyResultCompact()
	}
	full := m.renderApplyResultFull()
	if viewportMaxYOffset(full, m.mainViewportHeight()) > 0 {
		return m.renderApplyResultCompact()
	}
	return full
}

func (m Model) renderApplyResultFull() string {
	result := m.applyResult.Result
	panelWidth := calcFullWidth(m.width)
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.apply.title")))
	b.WriteString("  ")
	b.WriteString(badgeOnlineStyle.Render(m.t("tui.state.success")))
	b.WriteString("\n\n")
	b.WriteString(kvWrapped(m.t("tui.apply.revision"), m.applyResult.Revision, panelInnerWidth(panelWidth)))
	b.WriteString(kv(m.t("tui.apply.applied"), fmt.Sprintf("%d", result.AppliedRules)))
	b.WriteString(kv(m.t("tui.apply.stopped"), fmt.Sprintf("%d", result.StoppedRules)))
	b.WriteString(kv(m.t("tui.apply.failed"), fmt.Sprintf("%d", result.FailedRules)))
	b.WriteString(kv(m.t("tui.apply.total"), fmt.Sprintf("%d", result.TotalRules)))
	if len(result.Items) > 0 {
		b.WriteString("\n")
		b.WriteString(panelTitle("", m.t("tui.panel.results")))
		b.WriteString("\n")
		start, end := m.visibleApplyResultRange(len(result.Items))
		for index := start; index < end; index++ {
			item := result.Items[index]
			line := strings.Join([]string{
				cellColumn(item.RuleID, 20),
				cellColumn(m.localizedApplyAction(item.Action), 16),
				m.localizedApplyStatus(item.Status),
			}, " ")
			if item.Error != "" {
				line += "  " + safeText(item.Error)
			}
			line = truncate(line, max(panelInnerWidth(panelWidth)-2, 1))
			if index == m.resultSelected {
				line = selectedStyle.Render("> " + line)
			} else {
				line = "  " + line
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderApplyResultCompact() string {
	result := m.applyResult.Result
	panelWidth := calcFullWidth(m.width)
	innerWidth := panelInnerWidth(panelWidth)
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.apply.title")))
	b.WriteString("  ")
	b.WriteString(badgeOnlineStyle.Render(m.t("tui.state.success")))
	b.WriteString("\n")
	b.WriteString(m.t("tui.apply.revision_short") + " ")
	b.WriteString(valueStyle.Render(truncate(m.applyResult.Revision, max(innerWidth-4, 1))))
	b.WriteString("\n")
	b.WriteString(m.tf("tui.apply.compact_applied", result.AppliedRules, result.StoppedRules) + "\n")
	b.WriteString(m.tf("tui.apply.compact_failed", result.FailedRules, result.TotalRules))
	if len(result.Items) == 0 {
		return panelStyle.Width(panelWidth).Render(b.String())
	}
	b.WriteString("\n")
	b.WriteString(panelTitle("", m.t("tui.panel.results")))
	b.WriteString("\n")
	maxRows := m.applyResultPageSize()
	start := max(0, min(m.resultSelected-maxRows/2, len(result.Items)-maxRows))
	end := min(start+maxRows, len(result.Items))
	for index := start; index < end; index++ {
		item := result.Items[index]
		line := fmt.Sprintf("%s %s %s", item.RuleID, m.localizedApplyAction(item.Action), m.localizedApplyStatus(item.Status))
		if item.Error != "" {
			line += ": " + item.Error
		}
		line = truncate(line, max(innerWidth-2, 1))
		if index == m.resultSelected {
			line = selectedStyle.Render("> " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line)
		if index < end-1 {
			b.WriteString("\n")
		}
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) localizedApplyAction(action engine.ApplyAction) string {
	var key string
	switch strings.ToLower(strings.TrimSpace(string(action))) {
	case "start", "started":
		key = "tui.apply.action.started"
	case "restart", "restarted":
		key = "tui.apply.action.restarted"
	case "stop", "stopped":
		key = "tui.apply.action.stopped"
	case "remove", "removed":
		key = "tui.apply.action.removed"
	case "none", "unchanged":
		key = "tui.apply.action.unchanged"
	case "fail", "failed":
		key = "tui.apply.action.failed"
	default:
		return safeText(string(action))
	}
	return m.t(key)
}

func (m Model) localizedApplyStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "success":
		return m.t("tui.apply.status.ok")
	case "failed", "error":
		return m.t("tui.apply.status.failed")
	default:
		return safeText(status)
	}
}

func (m Model) renderBotConfig() string {
	if m.width < 50 || m.height < 16 {
		return m.renderBotConfigCompact()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.bot.title")))
	b.WriteString("\n\n")
	if m.botConfig == nil {
		if m.botConfigErr != nil {
			b.WriteString(badgeOfflineStyle.Render(m.t("tui.state.unavailable")))
			b.WriteString("\n\n")
			message := m.tf("tui.bot.load_failed", m.friendlyError(m.botConfigErr))
			b.WriteString(badgeOfflineStyle.Render(ansi.Wrap(message, max(panelInnerWidth(calcFullWidth(m.width)), 1), "")))
			b.WriteString("\n\n")
			b.WriteString(valueStyle.Render(m.t("tui.bot.retry")))
		} else {
			b.WriteString(subtleStyle.Render(m.t("tui.bot.loading")))
		}
		return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
	}
	cfg := m.botConfig
	status := badgeOfflineStyle.Render(m.t("tui.state.stopped"))
	if cfg.Running {
		status = badgeOnlineStyle.Render(m.t("tui.state.running"))
	}
	b.WriteString(kvStyled(m.t("tui.label.status"), status))
	botToken := m.t("tui.value.not_set")
	if strings.TrimSpace(cfg.BotToken) != "" {
		botToken = m.t("tui.value.configured")
	}
	b.WriteString(kv(m.t("tui.bot.token"), botToken))
	chatID := m.t("tui.value.not_set_parenthesized")
	if cfg.BotChat != 0 {
		chatID = fmt.Sprintf("%d", cfg.BotChat)
	}
	b.WriteString(kv(m.t("tui.bot.chat_id"), chatID))
	controlMode := badgePausedStyle.Render(m.t("tui.state.read_only"))
	if strings.TrimSpace(cfg.BotControlToken) != "" {
		controlMode = badgeOnlineStyle.Render(m.t("tui.state.token_set"))
	}
	b.WriteString(kvStyled(m.t("tui.bot.control"), controlMode))
	if m.botConfigErr != nil {
		b.WriteString(kvStyled(m.t("tui.bot.freshness"), badgeOfflineStyle.Render(m.t("tui.state.stale"))))
		if !m.botLastUpdated.IsZero() {
			b.WriteString(kv(m.t("tui.bot.last_update"), m.botLastUpdated.Format("15:04:05")))
		}
		message := m.tf("tui.bot.refresh_failed", m.friendlyError(m.botConfigErr))
		b.WriteString("\n")
		b.WriteString(badgeOfflineStyle.Render(ansi.Wrap(message, max(panelInnerWidth(calcFullWidth(m.width)), 1), "")))
	}
	if m.botOperation == botOperationFetching {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.spinner.View() + " " + m.t("tui.bot.refreshing")))
	}
	b.WriteString("\n")
	if m.botConfigErr != nil {
		b.WriteString(valueStyle.Render(m.t("tui.bot.retry_before_change")))
	} else if !botReadyToStart(cfg) {
		missing := m.t("tui.bot.missing_both")
		switch {
		case strings.TrimSpace(cfg.BotToken) != "":
			missing = m.t("tui.bot.missing_chat_id")
		case cfg.BotChat != 0:
			missing = m.t("tui.bot.missing_token")
		}
		b.WriteString(subtleStyle.Render(m.tf("tui.bot.configure_missing", missing)))
	} else if cfg.Running {
		b.WriteString(subtleStyle.Render(strings.Join(m.localizedHints([]string{"[e]edit", "[x]stop", "[r]refresh"}), "  ")))
	} else {
		b.WriteString(subtleStyle.Render(strings.Join(m.localizedHints([]string{"[e]edit", "[s]start", "[r]refresh"}), "  ")))
	}
	b.WriteString("\n")
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderBotConfigCompact() string {
	panelWidth := calcFullWidth(m.width)
	innerWidth := panelInnerWidth(panelWidth)
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.bot.title")))
	if m.botConfig == nil {
		b.WriteString("\n")
		if m.botConfigErr == nil {
			b.WriteString(subtleStyle.Render(m.t("tui.bot.loading_compact")))
		} else {
			b.WriteString(badgeOfflineStyle.Render(m.t("tui.state.unavailable")))
			b.WriteString("\n")
			b.WriteString(badgeOfflineStyle.Render(truncate(m.friendlyError(m.botConfigErr), innerWidth)))
		}
		return panelStyle.Width(panelWidth).Render(b.String())
	}
	status := badgeOfflineStyle.Render(m.t("tui.state.stopped"))
	if m.botConfig.Running {
		status = badgeOnlineStyle.Render(m.t("tui.state.running"))
	}
	if m.botConfigErr != nil {
		status += " " + badgeOfflineStyle.Render(m.t("tui.state.stale"))
	}
	token := m.t("tui.value.not_set")
	if m.botConfig.BotToken != "" {
		token = m.t("tui.value.set")
	}
	chatID := m.t("tui.value.not_set")
	if m.botConfig.BotChat != 0 {
		chatID = fmt.Sprintf("%d", m.botConfig.BotChat)
	}
	b.WriteString("\n")
	if m.botOperation == botOperationFetching {
		status += " " + subtleStyle.Render(m.spinner.View()+" "+m.t("tui.operation.refresh"))
	}
	b.WriteString(compactKVStyled(m.t("tui.label.status"), status))
	b.WriteString("\n")
	b.WriteString(compactKV(m.t("tui.bot.token_short"), token))
	b.WriteString("\n")
	b.WriteString(compactKV(m.t("tui.bot.chat_id"), chatID))
	b.WriteString("\n")
	controlMode := m.t("tui.value.read_only")
	if strings.TrimSpace(m.botConfig.BotControlToken) != "" {
		controlMode = m.t("tui.value.token_set")
	}
	b.WriteString(compactKV(m.t("tui.bot.control"), controlMode))
	if m.botConfigErr != nil {
		b.WriteString("\n")
		b.WriteString(badgeOfflineStyle.Render(truncate(m.tf("tui.bot.refresh_failed", m.friendlyError(m.botConfigErr)), innerWidth)))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func compactKV(key, value string) string {
	return compactKVStyled(key, valueStyle.Render(safeText(value)))
}

func compactKVStyled(key, value string) string {
	return "  " + labelStyle.Width(10).Render(safeText(key)+":") + " " + value
}

func (m Model) renderBotEditor() string {
	if m.botEditor == nil {
		return subtleStyle.Render(m.t("tui.editor.unavailable"))
	}
	e := m.botEditor
	panelWidth := calcFullWidth(m.width)
	inputWidth := max(panelInnerWidth(panelWidth)-19, 1)
	compact := m.width < 50 || m.height < 16
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.bot_editor.title")))
	b.WriteString("\n")
	b.WriteString(panelDivider(panelWidth, 70))
	b.WriteString("\n")
	fields := []struct{ label, view string }{
		{m.t("tui.bot_editor.field.token") + ":", e.inputView(0, inputWidth)},
		{m.t("tui.bot_editor.field.chat_id") + ":", e.inputView(1, inputWidth)},
		{m.t("tui.bot_editor.field.control_token") + ":", e.inputView(2, inputWidth)},
	}
	for i, f := range fields {
		prefix := "  "
		if i == e.focus {
			prefix = "> "
		}
		b.WriteString(prefix + cellColumn(f.label, 16) + " " + f.view)
		if i < len(fields)-1 || e.formError != "" || !compact {
			b.WriteString("\n")
		}
		if i == 1 && e.chatError != "" {
			b.WriteString(strings.Repeat(" ", 19))
			message := ansi.Wrap(e.chatError, max(inputWidth, 1), "")
			if compact {
				message = truncate(e.chatError, max(inputWidth, 1))
			}
			b.WriteString(badgeOfflineStyle.Render(message))
			b.WriteString("\n")
		}
	}
	if e.formError != "" {
		message := ansi.Wrap(safeText(e.formError), max(panelInnerWidth(panelWidth), 1), "")
		if compact {
			message = truncate(e.formError, max(panelInnerWidth(panelWidth), 1))
		} else {
			b.WriteString("\n")
		}
		b.WriteString(badgePausedStyle.Render(message))
		if !compact {
			b.WriteString("\n")
		}
	}
	if !compact {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.t("tui.bot_editor.token_note")))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

// friendlyError maps common network/daemon errors to a short human hint.
func friendlyError(err error) string {
	if err == nil {
		return ""
	}
	msg := safeText(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return "cannot reach daemon (is it running at this address?)"
	case strings.Contains(msg, "no such host"):
		return "unknown daemon address"
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "context deadline exceeded"):
		return "request timed out"
	case strings.Contains(msg, "EOF"):
		return "connection closed by daemon"
	default:
		return msg
	}
}

func (m Model) friendlyError(err error) string {
	if err == nil {
		return ""
	}
	msg := safeText(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return m.t("tui.network.connection_refused")
	case strings.Contains(msg, "no such host"):
		return m.t("tui.network.unknown_host")
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "context deadline exceeded"):
		return m.t("tui.network.timeout")
	case strings.Contains(msg, "EOF"):
		return m.t("tui.network.closed")
	default:
		return msg
	}
}

func (m Model) localizedSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error":
		return m.t("severity.error")
	case "warning":
		return m.t("severity.warning")
	default:
		return strings.ToUpper(value)
	}
}

func (m Model) localizedSeverityShort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error":
		return m.t("tui.severity.error_short")
	case "warning":
		return m.t("tui.severity.warning_short")
	default:
		return "?"
	}
}

func (m Model) localizedPrecheckCheck(check string) string {
	check = strings.ToLower(strings.TrimSpace(check))
	if check == "" {
		return m.t("tui.value.unknown")
	}
	key := "tui.precheck.check." + check
	value := m.t(key)
	if value == key {
		return strings.ReplaceAll(check, "_", " ")
	}
	return value
}

func (m Model) localizedPrecheckMessage(item precheck.Item) string {
	if m.tr == nil || m.tr.Locale() == i18n.English {
		return safeText(item.Message)
	}
	check := strings.ToLower(strings.TrimSpace(item.Check))
	key := "tui.precheck.message." + check
	value := m.t(key)
	if value == key {
		return safeText(item.Message)
	}
	return value
}

func (m Model) visibleApplyResultRange(count int) (int, int) {
	maxRows := m.applyResultPageSize()
	if count <= maxRows {
		return 0, count
	}
	start := m.resultSelected - maxRows/2
	start = max(0, min(start, count-maxRows))
	return start, start + maxRows
}

func (m Model) applyResultPageSize() int {
	if m.height < 18 {
		return max(m.mainViewportHeight()-7, 1)
	}
	return max(m.height-15, 3)
}

func (m Model) renderOverlay() string {
	switch m.overlay {
	case overlayHelp:
		return m.renderHelp()
	case overlayLanguage:
		return m.renderLanguageSelector()
	case overlayReload:
		return m.renderConfirm(m.t("tui.confirm.reload.title"), m.t("tui.confirm.reload.prompt"), m.t("tui.confirm.reload.detail"))
	case overlayDelete:
		name := m.t("tui.confirm.delete.selected_rule")
		if rule := m.selectedRule(); rule != nil {
			name = safeText(rule.RuleID)
		}
		return m.renderConfirm(m.t("tui.confirm.delete.title"), m.tf("tui.confirm.delete.prompt", name), m.t("tui.confirm.delete.detail"))
	case overlayApply:
		return m.renderApplyConfirm()
	case overlayDiscardDraft:
		return m.renderConfirm(m.t("tui.confirm.discard.title"), m.t("tui.confirm.discard.prompt"), m.t("tui.confirm.discard.detail"))
	case overlayQuitDirty:
		return m.renderConfirm(m.t("tui.confirm.quit.title"), m.t("tui.confirm.quit.prompt"), m.tf("tui.confirm.quit.detail", m.dirtyChangeCount()))
	case overlayCancelEditor:
		return m.renderConfirm(m.t("tui.confirm.cancel_editor.title"), m.t("tui.confirm.cancel_editor.prompt"), m.t("tui.confirm.cancel_editor.detail"))
	case overlayCancelBotEditor:
		return m.renderConfirm(m.t("tui.confirm.cancel_bot.title"), m.t("tui.confirm.cancel_editor.prompt"), m.t("tui.confirm.cancel_bot.detail"))
	case overlayBotStop:
		return m.renderConfirm(m.t("tui.confirm.stop_bot.title"), m.t("tui.confirm.stop_bot.prompt"), m.t("tui.confirm.stop_bot.detail"))
	case overlayStartService:
		detail := m.t("tui.confirm.start_service.detail")
		if m.width < 60 {
			detail = m.t("tui.confirm.start_service.detail_compact")
		}
		return m.renderConfirm(m.t("tui.confirm.start_service.title"), m.t("tui.confirm.start_service.prompt"), detail)
	case overlayElevateService:
		return m.renderConfirm(m.t("tui.confirm.elevate_service.title"), m.t("tui.confirm.elevate_service.prompt"), m.t("tui.confirm.elevate_service.detail"))
	case overlayUDPSettings:
		return m.renderUDPSettings()
	default:
		return ""
	}
}

func (m Model) renderConfirm(title, prompt, detail string) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(safeText(title)))
	b.WriteString("\n\n")
	b.WriteString(safeText(prompt))
	if detail != "" {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(safeText(detail)))
	}
	b.WriteString("\n\n")
	if m.operation != operationIdle {
		b.WriteString(valueStyle.Render(m.operationLabel()))
	} else {
		b.WriteString(valueStyle.Render(m.t("tui.confirm.actions")))
	}
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderLanguageSelector() string {
	panelWidth := calcFullWidth(m.width)
	contentWidth := panelInnerWidth(panelWidth)
	current := i18n.English
	if m.tr != nil {
		current = m.tr.Locale()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.language.title")))
	b.WriteString("\n\n")
	b.WriteString(ansi.Wrap(safeText(m.t("tui.language.prompt")), contentWidth, ""))
	b.WriteString("\n\n")
	writeOption := func(number, label, locale string) {
		b.WriteString("  ")
		b.WriteString(valueStyle.Render("[" + number + "]"))
		b.WriteString(" ")
		b.WriteString(safeText(label))
		if current == locale {
			b.WriteString("  ")
			b.WriteString(badgeOnlineStyle.Render(m.t("tui.language.current")))
		}
		b.WriteString("\n")
	}
	writeOption("1", m.t("tui.language.option.english"), i18n.English)
	writeOption("2", m.t("tui.language.option.simplified_chinese"), i18n.SimplifiedChinese)
	b.WriteString("\n")
	b.WriteString(valueStyle.Render(ansi.Wrap(safeText(m.t("tui.language.actions")), contentWidth, "")))
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderApplyConfirm() string {
	full := m.renderApplyConfirmFull()
	if viewportMaxYOffset(full, max(m.height, 1)) > 0 {
		return m.renderApplyConfirmCompact()
	}
	return full
}

func (m Model) renderApplyConfirmFull() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.apply_confirm.title")))
	b.WriteString("\n\n")
	diff := m.localDiff()
	changes := m.sortedDiffValues(diff)
	changeLimit := min(len(changes), max(m.height-12, 3))
	for _, line := range changes[:changeLimit] {
		b.WriteString("  " + safeText(line) + "\n")
	}
	if changeLimit < len(changes) {
		b.WriteString(subtleStyle.Render(m.tf("tui.precheck.more_changes", len(changes)-changeLimit)))
		b.WriteString("\n")
	}
	if m.draft != nil && m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		b.WriteString(fmt.Sprintf("  ~ udp_max_sessions %d -> %d\n", m.config.UDPMaxSessions, m.draft.UDPMaxSessions))
	}
	warnings := 0
	if m.precheckResult != nil {
		warnings = m.precheckResult.Precheck.WarningCount
	}
	b.WriteString("\n")
	b.WriteString(badgePausedStyle.Render(m.tf("tui.apply_confirm.warnings", warnings)))
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(m.t("tui.apply_confirm.connection_warning")))
	b.WriteString("\n\n")
	b.WriteString(valueStyle.Render(m.t("tui.apply_confirm.actions")))
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderApplyConfirmCompact() string {
	changes := m.sortedDiffValues(m.localDiff())
	if m.draft != nil && m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		changes = append(changes, fmt.Sprintf("~ udp_max_sessions %d -> %d", m.config.UDPMaxSessions, m.draft.UDPMaxSessions))
	}
	warnings := 0
	if m.precheckResult != nil {
		warnings = m.precheckResult.Precheck.WarningCount
	}
	panelWidth := calcFullWidth(m.width)
	innerWidth := panelInnerWidth(panelWidth)
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.apply_confirm.title")))
	b.WriteString("\n")
	b.WriteString(m.tf("tui.apply_confirm.pending", len(changes)) + "\n")
	if len(changes) <= 2 {
		for _, line := range changes {
			b.WriteString("  " + truncate(line, max(innerWidth-2, 1)) + "\n")
		}
	} else {
		b.WriteString("  " + truncate(changes[0], max(innerWidth-2, 1)) + "\n")
		b.WriteString(subtleStyle.Render(m.tf("tui.precheck.more_changes", len(changes)-1) + "\n"))
	}
	b.WriteString(badgePausedStyle.Render(m.tf("tui.apply_confirm.warnings", warnings)))
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(truncate(m.t("tui.apply_confirm.connections_may_close"), innerWidth)))
	b.WriteString("\n")
	b.WriteString(valueStyle.Render(m.t("tui.apply_confirm.actions_compact")))
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderUDPSettings() string {
	panelWidth := calcFullWidth(m.width)
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.udp.title")))
	b.WriteString("\n\n")
	b.WriteString(m.t("tui.udp.limit") + ": ")
	b.WriteString(responsiveTextInputView(m.udpInput, panelInnerWidth(panelWidth)-7, 32))
	if m.udpInputError != "" {
		b.WriteString("\n")
		b.WriteString(badgeOfflineStyle.Render(m.udpInputError))
	}
	b.WriteString("\n\n")
	b.WriteString(valueStyle.Render(m.t("tui.udp.actions")))
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderHelp() string {
	shortcuts := m.contextualHelpShortcuts()
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.t("tui.help.title")))
	b.WriteString("\n\n")
	for _, shortcut := range shortcuts {
		b.WriteString(helpShortcutLine(shortcut.key, shortcut.description))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(m.t("tui.help.sync") + " "))
	b.WriteString(syncStyle(syncClean).Render(m.t("tui.sync.synced")) + " ")
	b.WriteString(syncStyle(syncDirty).Render(m.t("tui.sync.draft")) + " ")
	b.WriteString(syncStyle(syncValidated).Render(m.t("tui.sync.validated")) + " ")
	b.WriteString(syncStyle(syncStale).Render(m.t("tui.sync.stale")))
	b.WriteString(subtleStyle.Render(m.t("tui.help.local_edits_before")) + valueStyle.Render("A") + subtleStyle.Render(m.t("tui.help.local_edits_after")))
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

type helpShortcut struct {
	key         string
	description string
}

func (m Model) contextualHelpShortcuts() []helpShortcut {
	shortcuts := []helpShortcut{{"1 / 2 / 3", m.t("tui.help.navigation")}}
	switch m.view {
	case viewDashboard:
		shortcuts = append(shortcuts,
			helpShortcut{"j/k / PgUp/PgDn", m.t("tui.help.dashboard_scroll")},
			helpShortcut{"a", m.t("tui.help.network_toggle")},
			helpShortcut{"4", m.t("tui.help.network_jump")},
			helpShortcut{"r", m.t("tui.help.refresh")},
			helpShortcut{"p", m.t("tui.help.pause")},
			helpShortcut{"+ / -", m.t("tui.help.interval")},
			helpShortcut{"R", m.t("tui.help.reload")},
		)
	case viewRules, viewDetail:
		shortcuts = append(shortcuts,
			helpShortcut{"up/down j/k", m.t("tui.help.move")},
			helpShortcut{"enter", m.t("tui.help.primary_or_detail")},
			helpShortcut{"n / e / c", m.t("tui.help.rule_edit")},
			helpShortcut{"Space / d", m.t("tui.help.stage")},
			helpShortcut{"P / A", m.t("tui.help.precheck_apply")},
			helpShortcut{"u", m.t("tui.help.discard")},
			helpShortcut{"/ / s", m.t("tui.help.filter_sort")},
			helpShortcut{"g", m.t("tui.help.udp")},
		)
	case viewEditor:
		shortcuts = append(shortcuts,
			helpShortcut{"Tab / Shift+Tab", m.t("tui.help.editor_field")},
			helpShortcut{"Left / Right", m.t("tui.help.editor_option")},
			helpShortcut{"F2", m.t("tui.help.editor_advanced")},
			helpShortcut{"Ctrl+S", m.t("tui.help.save_draft")},
		)
	case viewPrecheck:
		shortcuts = append(shortcuts,
			helpShortcut{"up/down j/k", m.t("tui.help.move")},
			helpShortcut{"Enter", m.t("tui.help.primary_action")},
			helpShortcut{"e", m.t("tui.help.edit_finding")},
			helpShortcut{"P", m.t("tui.help.recheck")},
		)
	case viewApplyResult:
		shortcuts = append(shortcuts,
			helpShortcut{"up/down j/k", m.t("tui.help.move")},
			helpShortcut{"Enter / Esc", m.t("tui.help.back_rules")},
		)
	case viewBotConfig, viewBotEditor:
		shortcuts = append(shortcuts,
			helpShortcut{"e", m.t("tui.help.bot_edit")},
			helpShortcut{"s / x", m.t("tui.help.bot_lifecycle")},
			helpShortcut{"r", m.t("tui.help.refresh")},
		)
	}
	return append(shortcuts,
		helpShortcut{"l", m.t("tui.help.language")},
		helpShortcut{"Esc", m.t("tui.help.back")},
		helpShortcut{"? / F1", m.t("tui.help.help")},
		helpShortcut{"q / Ctrl+C", m.t("tui.help.quit")},
	)
}

func (m Model) renderHelpFooter() string {
	hints := []string{"[j/k]scroll", "[pgup/pgdn]page", "[home/end]jump", "[l]anguage", "[esc/?/F1]close"}
	return footerStyle.Render(wrapHints(m.localizedHints(hints), max(m.width, 1)))
}

func helpShortcutLine(key, description string) string {
	return "  " + valueStyle.Width(16).Render(safeText(key)) + " " + subtleStyle.Render(safeText(description))
}

func (m Model) helpViewportHeight() int {
	return max(m.height-lipgloss.Height(m.renderHelpFooter()), 1)
}

func (m Model) mainViewportHeight() int {
	return max(m.height-lipgloss.Height(m.renderHeader())-lipgloss.Height(m.renderStatusLine())-lipgloss.Height(m.renderFooter()), 1)
}

func viewportMaxYOffset(content string, height int) int {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return max(strings.Count(content, "\n")+1-max(height, 1), 0)
}

func (m Model) helpMaxYOffset() int {
	return viewportMaxYOffset(m.renderHelp(), m.helpViewportHeight())
}

func (m Model) contentMaxYOffset() int {
	content := ""
	switch m.view {
	case viewDashboard:
		content = m.renderDashboard()
	case viewDetail:
		content = m.renderDetail()
	}
	return viewportMaxYOffset(content, m.mainViewportHeight())
}

// detailMaxYOffset is kept for detail-view callers and regression tests.
func (m Model) detailMaxYOffset() int {
	return viewportMaxYOffset(m.renderDetail(), m.mainViewportHeight())
}

func (m *Model) scrollHelp(delta int) {
	limit := m.helpMaxYOffset()
	current := max(0, min(m.overlayYOffset, limit))
	m.overlayYOffset = max(0, min(current+delta, limit))
}

func (m *Model) scrollContent(delta int) {
	limit := m.contentMaxYOffset()
	current := max(0, min(m.contentYOffset, limit))
	m.contentYOffset = max(0, min(current+delta, limit))
}

func (m *Model) clampScrollOffsets() {
	m.contentYOffset = max(0, min(m.contentYOffset, m.contentMaxYOffset()))
	m.overlayYOffset = max(0, min(m.overlayYOffset, m.helpMaxYOffset()))
}

func panelTitle(iconKey, name string) string {
	icon := PanelIcons[iconKey]
	if icon == "" {
		return titleStyle.Render(name)
	}
	return fmt.Sprintf("%s %s", icon, titleStyle.Render(name))
}

func kv(key, value string) string {
	return kvStyled(key, valueStyle.Render(safeText(value)))
}

func kvStyled(key, value string) string {
	return "  " + labelStyle.Width(14).Render(safeText(key)+":") + " " + value + "\n"
}

func kvWrapped(key, value string, width int) string {
	label := labelStyle.Width(14).Render(safeText(key) + ":")
	prefix := "  " + label + " "
	prefixWidth := 17
	wrapped := ansi.Wrap(safeText(value), max(width-prefixWidth, 1), "")
	lines := strings.Split(wrapped, "\n")
	var b strings.Builder
	for index, line := range lines {
		if index == 0 {
			b.WriteString(prefix)
		} else {
			b.WriteString(strings.Repeat(" ", prefixWidth))
		}
		b.WriteString(valueStyle.Render(line))
		b.WriteString("\n")
	}
	return b.String()
}

func renderSparkline(data []float64, color lipgloss.TerminalColor, width int) string {
	if len(data) == 0 || width <= 0 {
		return ""
	}
	maxValue := 0.0
	for _, value := range data {
		if value > maxValue {
			maxValue = value
		}
	}
	if maxValue == 0 {
		maxValue = 1
	}
	start := 0
	if len(data) > width {
		start = len(data) - width
	}
	style := lipgloss.NewStyle().Foreground(color)
	var b strings.Builder
	for _, value := range data[start:] {
		level := int(math.Round(value / maxValue * float64(len(sparklineChars)-1)))
		level = max(0, min(level, len(sparklineChars)-1))
		b.WriteString(style.Render(string(sparklineChars[level])))
	}
	return b.String()
}

func formatBytes(value int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case value >= TB:
		return fmt.Sprintf("%.1fT", float64(value)/float64(TB))
	case value >= GB:
		return fmt.Sprintf("%.1fG", float64(value)/float64(GB))
	case value >= MB:
		return fmt.Sprintf("%.1fM", float64(value)/float64(MB))
	case value >= KB:
		return fmt.Sprintf("%.1fK", float64(value)/float64(KB))
	default:
		return fmt.Sprintf("%dB", value)
	}
}

func formatNetworkBytes(value uint64) string {
	const (
		KB = uint64(1024)
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
		PB = TB * 1024
	)
	switch {
	case value >= PB:
		return fmt.Sprintf("%.1fP", float64(value)/float64(PB))
	case value >= TB:
		return fmt.Sprintf("%.1fT", float64(value)/float64(TB))
	case value >= GB:
		return fmt.Sprintf("%.1fG", float64(value)/float64(GB))
	case value >= MB:
		return fmt.Sprintf("%.1fM", float64(value)/float64(MB))
	case value >= KB:
		return fmt.Sprintf("%.1fK", float64(value)/float64(KB))
	default:
		return fmt.Sprintf("%dB", value)
	}
}

func formatRate(bytesPerSecond float64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytesPerSecond >= GB:
		return fmt.Sprintf("%.1fG/s", bytesPerSecond/GB)
	case bytesPerSecond >= MB:
		return fmt.Sprintf("%.1fM/s", bytesPerSecond/MB)
	case bytesPerSecond >= KB:
		return fmt.Sprintf("%.1fK/s", bytesPerSecond/KB)
	default:
		return fmt.Sprintf("%.0fB/s", bytesPerSecond)
	}
}

func truncate(value string, maxLength int) string {
	if maxLength <= 0 {
		return ""
	}
	value = safeText(value)
	if maxLength <= 2 {
		return ansi.Truncate(value, maxLength, "")
	}
	return ansi.Truncate(value, maxLength, "..")
}

func cellColumn(value string, width int) string {
	value = truncate(value, width)
	return value + strings.Repeat(" ", max(width-lipgloss.Width(value), 0))
}

func panelDivider(panelWidth, limit int) string {
	width := panelInnerWidth(panelWidth)
	if limit > 0 {
		width = min(width, limit)
	}
	return subtleStyle.Render(strings.Repeat("-", width))
}

func formatEndpoint(host string, port int) string {
	return net.JoinHostPort(strings.TrimSpace(safeText(host)), strconv.Itoa(port))
}

func safeText(value string) string {
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\u2028', '\u2029':
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func boolStr(value bool) string {
	return boolStrWithTranslator(value, i18n.New(i18n.English))
}

func boolStrWithTranslator(value bool, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	if value {
		return translator.T("common.yes")
	}
	return translator.T("common.no")
}

func speedLimitStr(value int64) string {
	return speedLimitStrWithTranslator(value, i18n.New(i18n.English))
}

func speedLimitStrWithTranslator(value int64, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	if value <= 0 {
		return translator.T("tui.value.unlimited")
	}
	return formatRate(float64(value))
}

func maxConnStr(value int) string {
	return maxConnStrWithTranslator(value, i18n.New(i18n.English))
}

func maxConnStrWithTranslator(value int, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	if value <= 0 {
		return translator.T("tui.value.protocol_default")
	}
	return fmt.Sprintf("%d", value)
}

func sourceIPModeLabel(mode engine.SourceIPMode) string {
	return sourceIPModeLabelWithTranslator(mode, i18n.New(i18n.English))
}

func sourceIPModeLabelWithTranslator(mode engine.SourceIPMode, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	mode = engine.SourceIPMode(strings.ToLower(strings.TrimSpace(string(mode))))
	switch mode {
	case "", engine.SourceIPModeOff:
		return translator.T("tui.source_ip.off")
	case engine.SourceIPModeAllowlist:
		return translator.T("tui.source_ip.allowlist")
	case engine.SourceIPModeDenylist:
		return translator.T("tui.source_ip.denylist")
	default:
		return string(mode)
	}
}

func sourceIPPolicySummary(rule RuleInfo) string {
	return sourceIPPolicySummaryWithTranslator(rule, i18n.New(i18n.English))
}

func sourceIPPolicySummaryWithTranslator(rule RuleInfo, translator *i18n.Translator) string {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	switch engine.SourceIPMode(strings.ToLower(strings.TrimSpace(string(rule.SourceIPMode)))) {
	case engine.SourceIPModeAllowlist:
		return translator.Tf("tui.source_ip.allow_count", len(rule.SourceIPs))
	case engine.SourceIPModeDenylist:
		return translator.Tf("tui.source_ip.deny_count", len(rule.SourceIPs))
	default:
		return translator.T("tui.source_ip.open")
	}
}

func timeSince(value time.Time) time.Duration {
	return time.Since(value)
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
