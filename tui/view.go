package tui

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
)

func (m Model) View() string {
	if !m.ready {
		return m.spinner.View() + " initializing..."
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
	if m.view == viewDetail {
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
		"Terminal too small",
		fmt.Sprintf("minimum 40x12, current %dx%d", m.width, m.height),
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
	right := subtleStyle.Render(m.spinner.View())
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) viewName() string {
	switch m.view {
	case viewDashboard:
		return "DASHBOARD"
	case viewRules:
		return "RULES"
	case viewDetail:
		return "DETAIL"
	case viewEditor:
		return "EDITOR"
	case viewPrecheck:
		return "PRECHECK"
	case viewApplyResult:
		return "APPLY RESULT"
	case viewBotConfig:
		return "BOT"
	case viewBotEditor:
		return "BOT EDITOR"
	default:
		return ""
	}
}

func (m Model) renderBadge() string {
	if m.connectionErr != nil {
		return badgeOfflineStyle.Render("* OFFLINE")
	}
	if m.stats == nil {
		return badgePausedStyle.Render("* CONNECTING")
	}
	return badgeOnlineStyle.Render("* ONLINE")
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
			message = "connection: " + friendlyError(m.connectionErr)
		case m.configErr != nil:
			message = "config: " + friendlyError(m.configErr)
		case m.rulesErr != nil:
			message = "rules: " + friendlyError(m.rulesErr)
		}
		if message != "" {
			line := m.renderBadge() + subtleStyle.Render(" | ") + valueStyle.Render(safeText(message))
			return statusLineStyle.Render(ansi.Truncate(line, max(m.width, 1), ".."))
		}
	}
	parts := []string{
		m.renderBadge(),
		subtleStyle.Render("|"),
		subtleStyle.Render(safeText(m.roleLabel())),
		subtleStyle.Render("|"),
		syncStyle(m.sync).Render(m.syncLabel()),
	}
	if m.view == viewRules {
		parts = append(parts, subtleStyle.Render("|"), subtleStyle.Render("sort "+sortKeyLabel(m.sort)))
	}
	if m.operation != operationIdle {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(m.operationLabel()))
	}
	if m.botOperation != botOperationIdle {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(m.botOperationLabel()))
	}
	if m.paused {
		parts = append(parts, badgePausedStyle.Render("PAUSED"))
	}
	if recentStatus {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(truncate(m.statusText, 72)))
	}
	if m.connectionErr != nil {
		parts = append(parts, subtleStyle.Render("|"), badgeOfflineStyle.Render(truncate("connection: "+friendlyError(m.connectionErr), 60)))
	} else if m.configErr != nil {
		parts = append(parts, subtleStyle.Render("|"), badgeOfflineStyle.Render(truncate("config: "+friendlyError(m.configErr), 60)))
	} else if m.rulesErr != nil {
		parts = append(parts, subtleStyle.Render("|"), badgeOfflineStyle.Render(truncate("rules: "+friendlyError(m.rulesErr), 60)))
	}
	return statusLineStyle.MaxWidth(max(m.width, 1)).Render(strings.Join(parts, " "))
}

func (m Model) botOperationLabel() string {
	switch m.botOperation {
	case botOperationFetching:
		return m.spinner.View() + " BOT REFRESH"
	case botOperationSaving:
		return m.spinner.View() + " BOT SAVE"
	case botOperationStarting:
		return m.spinner.View() + " BOT START"
	case botOperationStopping:
		return m.spinner.View() + " BOT STOP"
	default:
		return ""
	}
}

func (m Model) operationLabel() string {
	switch m.operation {
	case operationPrechecking:
		return m.spinner.View() + " PRECHECKING"
	case operationApplying:
		return m.spinner.View() + " APPLYING"
	case operationReloading:
		return m.spinner.View() + " RELOADING"
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
		if m.canWrite() {
			hints = append(hints, "[b]ot")
		}
		hints = append(hints, "[tab]rules", "[p]ause", "[r]efresh")
		if m.canReload() {
			hints = append(hints, "[R]eload")
		}
		hints = append(hints, "[?]help", "[q]uit")
	case viewRules:
		if m.canWrite() {
			hints = append(hints, "[n]ew", "[e]dit", "[c]opy", "[space]toggle", "[d]elete", "[g]lobal", "[P]recheck", "[A]pply", "[u]discard", "[b]ot")
		}
		hints = append(hints, "[/]filter", "[s]ort", "[enter]detail", "[tab]dashboard", "[p]ause", "[r]efresh")
		if m.canReload() {
			hints = append(hints, "[R]eload")
		}
		hints = append(hints, "[?]help", "[q]uit")
	case viewDetail:
		if m.canWrite() {
			hints = append(hints, "[e]dit", "[c]opy", "[space]toggle", "[d]elete", "[P]recheck", "[A]pply", "[b]ot")
		}
		hints = append(hints, "[j/k]scroll", "[pgup/pgdn]page", "[home/end]jump", "[esc]rules", "[tab]dashboard", "[p]ause", "[r]efresh")
		if m.canReload() {
			hints = append(hints, "[R]eload")
		}
		hints = append(hints, "[?]help", "[q]uit")
	case viewEditor:
		hints = []string{"[tab/up/down]field", "[left/right]option", "[space]toggle", "[ctrl+s]save", "[esc]cancel", "[F1]help"}
	case viewPrecheck:
		hints = []string{"[up/down]finding", "[pgup/pgdn]page", "[enter/e]edit", "[P]recheck", "[A]pply", "[esc]rules", "[?]help", "[q]uit"}
	case viewApplyResult:
		hints = []string{"[up/down]result", "[pgup/pgdn]page", "[enter/esc]rules", "[tab]dashboard", "[p]ause", "[r]efresh"}
		if m.canReload() {
			hints = append(hints, "[R]eload")
		}
		hints = append(hints, "[?]help", "[q]uit")
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
		hints = append(hints, "[esc]rules", "[?]help", "[q]uit")
	case viewBotEditor:
		hints = []string{"[tab/shift+tab]field", "[ctrl+s]save", "[ctrl+r]rebase", "[esc]cancel", "[F1]help"}
	}
	return footerStyle.Render(wrapHints(hints, max(m.width, 1)))
}

func (m Model) renderCompactFooter() string {
	var hints []string
	switch m.view {
	case viewDashboard:
		hints = []string{"[tab]rules", "[r]refresh", "[?]help", "[q]quit"}
	case viewRules:
		if m.canWrite() {
			hints = append(hints, "[n]new", "[e]edit", "[P]check", "[A]apply")
		}
		hints = append(hints, "[enter]detail", "[?]help")
	case viewDetail:
		hints = []string{"[j/k]scroll", "[esc]rules", "[?]help"}
	case viewEditor:
		hints = []string{"[tab]field", "[ctrl+s]save", "[esc]cancel"}
	case viewPrecheck:
		hints = []string{"[j/k]finding", "[A]apply", "[esc]rules"}
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
		hints = append(hints, "[esc]rules", "[?]help")
	case viewBotEditor:
		hints = []string{"[tab]field", "[ctrl+s]save", "[ctrl+r]rebase", "[esc]cancel"}
	}
	return footerStyle.Render(wrapHints(hints, max(m.width, 1)))
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
	if m.stats == nil && m.rules == nil && m.config == nil {
		if err := m.dataLoadError(); err != nil {
			return m.renderEmptyPanel("Daemon unavailable", friendlyError(err), true)
		}
		return m.renderEmptyPanel("Connecting", "Waiting for the first daemon response.", false)
	}
	if m.height < 18 {
		return m.renderDashboardMinimal()
	}
	layout := detectLayout(m.width)
	if layout <= layoutCompact || m.height < 28 {
		compact := m.renderDashboardCompact()
		if viewportMaxYOffset(compact, m.mainViewportHeight()) > 0 {
			return m.renderDashboardMinimal()
		}
		return compact
	}
	system := m.renderSystemPanel()
	traffic := m.renderTrafficPanel()
	topRules := m.renderTopRulesPanel()
	var topRow string
	if layout >= layoutMedium {
		leftWidth, rightWidth := calcRowWidths(m.width)
		topRow = lipgloss.JoinHorizontal(lipgloss.Top,
			panelStyle.Width(leftWidth).Render(system),
			panelStyle.Width(rightWidth).Render(traffic),
		)
	} else {
		width := calcFullWidth(m.width)
		topRow = panelStyle.Width(width).Render(system) + "\n" + panelStyle.Width(width).Render(traffic)
	}
	return topRow + "\n" + panelStyle.Width(calcFullWidth(m.width)).Render(topRules)
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
	b.WriteString(titleStyle.Render("Overview"))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  Up %-8s %s\n", formatBytes(totalUpload), formatRate(uploadRate)))
	b.WriteString(fmt.Sprintf("  Down %-6s %s\n", formatBytes(totalDownload), formatRate(downloadRate)))
	b.WriteString(fmt.Sprintf("  Connections %-6d\n", connections))
	b.WriteString(fmt.Sprintf("  Rules %d running / %d configured", running, len(m.displayRules())))
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderDashboardCompact() string {
	width := calcFullWidth(m.width)
	return panelStyle.Width(width).Render(m.renderTrafficPanel()) + "\n" +
		panelStyle.Width(width).Render(m.renderTopRulesPanel())
}

func (m Model) renderSystemPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("System"))
	b.WriteString("\n")
	status := "CONNECTING"
	if m.connectionErr != nil {
		status = "OFFLINE"
	} else if m.stats != nil {
		status = "ONLINE"
	}
	running := 0
	if m.rules != nil {
		running = len(m.rules.Items)
	}
	b.WriteString(kv("Status", status))
	b.WriteString(kv("Running", fmt.Sprintf("%d rules", running)))
	b.WriteString(kv("Configured", fmt.Sprintf("%d rules", len(m.displayRules()))))
	b.WriteString(kv("Server", m.client.BaseURL()))
	return b.String()
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
	status := badgePausedStyle.Render("WAITING")
	if failed {
		status = badgeOfflineStyle.Render("OFFLINE")
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
	b.WriteString(panelTitle("Traffic"))
	b.WriteString("\n")
	var totalUpload, totalDownload, connections int64
	if m.stats != nil {
		for _, snapshot := range m.stats.Items {
			totalUpload += snapshot.UploadBytes
			totalDownload += snapshot.DownloadBytes
			connections += snapshot.Conns
		}
	}
	b.WriteString(kv("Total Upload", formatBytes(totalUpload)))
	b.WriteString(kv("Total Download", formatBytes(totalDownload)))
	b.WriteString(kv("Active Conns", fmt.Sprintf("%d", connections)))
	var uploadRate, downloadRate float64
	for _, rate := range m.rates {
		uploadRate += rate.UploadRate
		downloadRate += rate.DownloadRate
	}
	b.WriteString(kv("Upload Rate", formatRate(uploadRate)))
	b.WriteString(kv("Download Rate", formatRate(downloadRate)))
	return b.String()
}

func (m Model) renderTopRulesPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("Rules"))
	b.WriteString("\n")
	rules := m.displayRules()
	if len(rules) == 0 {
		message := "no configured rules"
		if m.config == nil && m.rules == nil {
			if m.dataLoadError() != nil {
				message = "rules unavailable"
			} else {
				message = "loading rules"
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
			line = fmt.Sprintf("%s %s %s %5d  up %8s down %8s",
				cellColumn(rule.Name, 14), cellColumn(m.ruleState(rule), 8), protocolLabel(string(rule.Protocol)), snapshot.Conns,
				formatRate(rate.UploadRate), formatRate(rate.DownloadRate))
		}
		b.WriteString(valueStyle.Render(line))
		b.WriteString("\n")
	}
	return b.String()
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
		b.WriteString(subtleStyle.Render("filter: " + truncate(filterValue, panelInnerWidth(fullWidth)-8)))
		b.WriteString("\n")
	}
	udpLimit := 0
	if m.draft != nil {
		udpLimit = m.draft.UDPMaxSessions
	} else if m.config != nil {
		udpLimit = m.config.UDPMaxSessions
	}
	if m.width < 50 {
		if filterValue != "" {
			b.WriteString(subtleStyle.Render(fmt.Sprintf("%d/%d rules  |  UDP max %d", len(rules), len(allRules), udpLimit)))
		} else {
			b.WriteString(subtleStyle.Render(fmt.Sprintf("%d rules  |  UDP max %d", len(allRules), udpLimit)))
		}
	} else if filterValue != "" {
		b.WriteString(subtleStyle.Render(fmt.Sprintf("Showing %d of %d  |  UDP max sessions %d", len(rules), len(allRules), udpLimit)))
	} else {
		b.WriteString(subtleStyle.Render(fmt.Sprintf("Configured %d  |  UDP max sessions %d", len(allRules), udpLimit)))
	}
	b.WriteString("\n")
	if len(rules) == 0 {
		b.WriteString("\n")
		switch {
		case filterValue != "" && len(allRules) > 0:
			b.WriteString(subtleStyle.Render("No matches for " + strconv.Quote(truncate(filterValue, 32)) + "."))
		case m.config == nil && m.rules == nil && m.dataLoadError() != nil:
			b.WriteString(badgeOfflineStyle.Render("Rules unavailable."))
			b.WriteString("\n")
			b.WriteString(subtleStyle.Render(friendlyError(m.dataLoadError())))
		case m.config == nil && m.rules == nil:
			b.WriteString(subtleStyle.Render("Loading rules..."))
		default:
			b.WriteString(subtleStyle.Render("No configured rules."))
			b.WriteString("\n")
			if m.canWrite() {
				b.WriteString(subtleStyle.Render("Press [n] to create one."))
			} else {
				b.WriteString(subtleStyle.Render("This session is read-only; ask an admin to add rules."))
			}
		}
		return panelStyle.Width(fullWidth).Render(b.String())
	}

	layout := detectLayout(m.width)
	switch {
	case m.width < 50:
		b.WriteString(fmt.Sprintf("%-12s %-8s %-7s\n", "NAME", "STATE", "PROTO"))
	case layout >= layoutWide:
		b.WriteString(fmt.Sprintf("%-16s %-9s %-9s %-7s %-20s %-20s %-10s %6s %9s %9s\n",
			"NAME", "STATE", "CHANGE", "PROTO", "LISTEN", "TARGET", "ACCESS", "CONNS", "UP/S", "DOWN/S"))
	case layout >= layoutMedium:
		b.WriteString(fmt.Sprintf("%-16s %-9s %-9s %-7s %-18s %-18s %6s\n",
			"NAME", "STATE", "CHANGE", "PROTO", "LISTEN", "TARGET", "CONNS"))
	case layout >= layoutNarrow:
		b.WriteString(fmt.Sprintf("%-15s %-9s %-9s %-7s %-17s %6s\n",
			"NAME", "STATE", "CHANGE", "PROTO", "LISTEN", "CONNS"))
	default:
		b.WriteString(fmt.Sprintf("%-12s %-8s %-7s %-7s\n", "NAME", "STATE", "CHANGE", "PROTO"))
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
				cellColumn(listen, 20), cellColumn(target, 20), cellColumn(sourceIPPolicySummary(rule), 10), snapshot.Conns,
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
		b.WriteString(subtleStyle.Render(fmt.Sprintf("rows %d-%d of %d", start+1, end, len(rules))))
	}
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
	if m.height < 16 && strings.TrimSpace(m.filterInput.Value()) != "" {
		return 2
	}
	return maxRows
}

func (m Model) renderDetail() string {
	rule := m.selectedRule()
	if rule == nil {
		return subtleStyle.Render("No rule selected.")
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
	b.WriteString(kvWrapped("Rule ID", rule.RuleID, innerWidth))
	b.WriteString(kvWrapped("Protocol", string(rule.Protocol), innerWidth))
	b.WriteString(kvWrapped("Listen", formatEndpoint(rule.ListenAddr, rule.ListenPort), innerWidth))
	b.WriteString(kvWrapped("Target", formatEndpoint(rule.TargetAddr, rule.TargetPort), innerWidth))
	b.WriteString(kvWrapped("Enabled", boolStr(rule.Enabled), innerWidth))
	b.WriteString(kvWrapped("Speed Limit", speedLimitStr(rule.SpeedLimit), innerWidth))
	b.WriteString(kvWrapped("Max Conns", maxConnStr(rule.MaxConn), innerWidth))
	b.WriteString(kvWrapped("Idle Timeout", fmt.Sprintf("%ds", rule.IdleTimeout), innerWidth))
	b.WriteString(kvWrapped("IP Access", sourceIPModeLabel(rule.SourceIPMode), innerWidth))
	if len(rule.SourceIPs) > 0 {
		b.WriteString(kvWrapped("Source IPs", strings.Join(rule.SourceIPs, ", "), innerWidth))
	}
	if rule.Remark != "" {
		b.WriteString(kvWrapped("Remark", rule.Remark, innerWidth))
	}
	stats := m.statsMap()[rule.RuleID]
	rate := m.rates[rule.RuleID]
	b.WriteString("\n")
	b.WriteString(panelTitle("Traffic"))
	b.WriteString("\n")
	b.WriteString(kvWrapped("Upload", fmt.Sprintf("%s (%s)", formatBytes(stats.UploadBytes), formatRate(rate.UploadRate)), innerWidth))
	b.WriteString(kvWrapped("Download", fmt.Sprintf("%s (%s)", formatBytes(stats.DownloadBytes), formatRate(rate.DownloadRate)), innerWidth))
	b.WriteString(kvWrapped("Connections", fmt.Sprintf("%d", stats.Conns), innerWidth))
	b.WriteString(kvWrapped("IP Denied", fmt.Sprintf("%d", stats.SourceIPDenied), innerWidth))
	if history := m.history[rule.RuleID]; history != nil && len(history.uploadRates) > 1 && m.height >= 24 && historyHasTraffic(history) {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("Upload rate"))
		b.WriteString("\n")
		b.WriteString(renderSparkline(history.uploadRates, CUpload, calcFullWidth(m.width)-6))
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("Download rate"))
		b.WriteString("\n")
		b.WriteString(renderSparkline(history.downloadRates, CDownload, calcFullWidth(m.width)-6))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
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
		return subtleStyle.Render("Editor unavailable.")
	}
	panelWidth := calcFullWidth(m.width)
	inputWidth := max(panelInnerWidth(panelWidth)-23, 1)
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.editor.title()))
	b.WriteString("\n")
	b.WriteString(panelDivider(panelWidth, 70))
	b.WriteString("\n")
	start, end := m.visibleEditorRange()
	hasRange := start > 0 || end < len(m.editor.fields)
	formError := m.editorFormErrorView()
	hasFormError := formError != ""
	for index := start; index < end; index++ {
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
			case editorProtocol, editorSourceIPMode:
				value = "< " + strings.ToUpper(field.choice) + " >"
			case editorToggle:
				if field.enabled {
					value = "[x] enabled"
				} else {
					value = "[ ] disabled"
				}
			case editorReadOnly:
				value = subtleStyle.Render(truncate(field.input.Value()+" (fixed)", inputWidth))
			default:
				if index == m.editor.focus {
					value = field.inputView(inputWidth)
				} else {
					value = truncate(field.input.Value(), inputWidth)
				}
			}
		}
		line := fmt.Sprintf("%s%-20s %s", prefix, field.label+":", value)
		b.WriteString(line)
		if index < end-1 || hasRange || hasFormError {
			b.WriteString("\n")
		}
	}
	if hasRange {
		b.WriteString(subtleStyle.Render(fmt.Sprintf("fields %d-%d of %d", start+1, end, len(m.editor.fields))))
		if hasFormError {
			b.WriteString("\n")
		}
	}
	if hasFormError {
		b.WriteString(badgeOfflineStyle.Render(formError))
	}
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) visibleEditorRange() (int, int) {
	if m.editor == nil {
		return 0, 0
	}
	available := m.mainViewportHeight()
	if formError := m.editorFormErrorView(); formError != "" {
		available -= lipgloss.Height(formError)
	}
	maxRows := max(available-4, 1)
	if len(m.editor.fields) > maxRows {
		maxRows = max(available-5, 1)
	}
	if len(m.editor.fields) <= maxRows {
		return 0, len(m.editor.fields)
	}
	start := m.editor.focus - maxRows/2
	start = max(0, min(start, len(m.editor.fields)-maxRows))
	return start, start + maxRows
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
		return subtleStyle.Render("No precheck result.")
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
	status := badgeOfflineStyle.Render("FAILED")
	if result.OK {
		status = badgeOnlineStyle.Render("PASSED")
	}
	b.WriteString(titleStyle.Render("Draft Precheck"))
	b.WriteString("  ")
	b.WriteString(status)
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%d rules  |  %d errors  |  %d warnings  |  %d ms\n",
		result.CheckedRules, result.ErrorCount, result.WarningCount, result.CheckedTimeMS))

	b.WriteString("\n")
	b.WriteString(panelTitle("Changes"))
	b.WriteString("\n")
	changes := precheckDiffValues(m.precheckResult.Diff)
	if len(changes) == 0 {
		changes = sortedDiffValues(m.localDiff())
	}
	if len(changes) == 0 && m.draft != nil && m.config != nil && m.draft.UDPMaxSessions == m.config.UDPMaxSessions {
		b.WriteString(subtleStyle.Render("no rule changes"))
		b.WriteString("\n")
	} else {
		changeLimit := min(len(changes), max(m.height/4, 3))
		for _, line := range changes[:changeLimit] {
			b.WriteString("  " + safeText(line) + "\n")
		}
		if changeLimit < len(changes) {
			b.WriteString(subtleStyle.Render(fmt.Sprintf("  ... %d more change(s)", len(changes)-changeLimit)))
			b.WriteString("\n")
		}
	}
	if m.draft != nil && m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		b.WriteString(fmt.Sprintf("  ~ udp_max_sessions %d -> %d\n", m.config.UDPMaxSessions, m.draft.UDPMaxSessions))
	}

	b.WriteString("\n")
	b.WriteString(panelTitle("Findings"))
	b.WriteString("\n")
	if len(result.Items) == 0 {
		b.WriteString(badgeOnlineStyle.Render("  no findings"))
		b.WriteString("\n")
	} else {
		b.WriteString(subtleStyle.Render(fmt.Sprintf("  %-7s %-18s %-20s %s\n", "SEVERITY", "RULE", "CHECK", "MESSAGE")))
		start, end := m.visibleFindingRange(len(result.Items))
		for index := start; index < end; index++ {
			item := result.Items[index]
			line := fmt.Sprintf("%-7s %-18s %-20s %s", strings.ToUpper(string(item.Severity)), truncate(item.RuleID, 18), item.Check, item.Message)
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
	status := badgeOfflineStyle.Render("FAILED")
	if result.OK {
		status = badgeOnlineStyle.Render("PASSED")
	}
	changes := precheckDiffValues(m.precheckResult.Diff)
	if len(changes) == 0 {
		changes = sortedDiffValues(m.localDiff())
	}
	changeCount := len(changes)
	if m.draft != nil && m.config != nil && m.draft.UDPMaxSessions != m.config.UDPMaxSessions {
		changeCount++
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("Draft Precheck"))
	b.WriteString("  ")
	b.WriteString(status)
	b.WriteString("\n")
	changeLabel := "changes"
	if changeCount == 1 {
		changeLabel = "change"
	}
	b.WriteString(fmt.Sprintf("%d rules | %d %s | E%d W%d\n", result.CheckedRules, changeCount, changeLabel, result.ErrorCount, result.WarningCount))
	b.WriteString(panelTitle("Findings"))
	if len(result.Items) == 0 {
		b.WriteString("\n")
		b.WriteString(badgeOnlineStyle.Render("  no findings"))
		return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
	}
	b.WriteString("\n")
	maxRows := m.precheckPageSize()
	start := max(0, min(m.resultSelected-maxRows/2, len(result.Items)-maxRows))
	end := min(start+maxRows, len(result.Items))
	lineWidth := max(panelInnerWidth(calcFullWidth(m.width))-2, 1)
	for index := start; index < end; index++ {
		item := result.Items[index]
		severity := strings.ToUpper(string(item.Severity))
		if len(severity) > 1 {
			severity = severity[:1]
		}
		line := truncate(fmt.Sprintf("%s %s %s: %s", severity, item.RuleID, item.Check, item.Message), lineWidth)
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
		return max(m.mainViewportHeight()-5, 1)
	}
	return max(m.height-17, 3)
}

func (m Model) renderApplyResult() string {
	if m.applyResult == nil {
		return subtleStyle.Render("No apply result.")
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
	b.WriteString(titleStyle.Render("Configuration Applied"))
	b.WriteString("  ")
	b.WriteString(badgeOnlineStyle.Render("SUCCESS"))
	b.WriteString("\n\n")
	b.WriteString(kvWrapped("Revision", m.applyResult.Revision, panelInnerWidth(panelWidth)))
	b.WriteString(kv("Applied", fmt.Sprintf("%d", result.AppliedRules)))
	b.WriteString(kv("Stopped", fmt.Sprintf("%d", result.StoppedRules)))
	b.WriteString(kv("Failed", fmt.Sprintf("%d", result.FailedRules)))
	b.WriteString(kv("Total", fmt.Sprintf("%d", result.TotalRules)))
	if len(result.Items) > 0 {
		b.WriteString("\n")
		b.WriteString(panelTitle("Results"))
		b.WriteString("\n")
		start, end := m.visibleApplyResultRange(len(result.Items))
		for index := start; index < end; index++ {
			item := result.Items[index]
			line := fmt.Sprintf("%-20s %-10s %s", truncate(item.RuleID, 20), safeText(string(item.Action)), safeText(item.Status))
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
	b.WriteString(titleStyle.Render("Configuration Applied"))
	b.WriteString("  ")
	b.WriteString(badgeOnlineStyle.Render("SUCCESS"))
	b.WriteString("\n")
	b.WriteString("Rev ")
	b.WriteString(valueStyle.Render(truncate(m.applyResult.Revision, max(innerWidth-4, 1))))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("Applied %d | Stopped %d\n", result.AppliedRules, result.StoppedRules))
	b.WriteString(fmt.Sprintf("Failed %d | Total %d", result.FailedRules, result.TotalRules))
	if len(result.Items) == 0 {
		return panelStyle.Width(panelWidth).Render(b.String())
	}
	b.WriteString("\n")
	b.WriteString(panelTitle("Results"))
	b.WriteString("\n")
	maxRows := m.applyResultPageSize()
	start := max(0, min(m.resultSelected-maxRows/2, len(result.Items)-maxRows))
	end := min(start+maxRows, len(result.Items))
	for index := start; index < end; index++ {
		item := result.Items[index]
		line := fmt.Sprintf("%s %s %s", item.RuleID, item.Action, item.Status)
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

func (m Model) renderBotConfig() string {
	if m.width < 50 || m.height < 16 {
		return m.renderBotConfigCompact()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Telegram Bot"))
	b.WriteString("\n\n")
	if m.botConfig == nil {
		if m.botConfigErr != nil {
			b.WriteString(badgeOfflineStyle.Render("UNAVAILABLE"))
			b.WriteString("\n\n")
			message := "Could not load bot configuration: " + friendlyError(m.botConfigErr)
			b.WriteString(badgeOfflineStyle.Render(ansi.Wrap(message, max(panelInnerWidth(calcFullWidth(m.width)), 1), "")))
			b.WriteString("\n\n")
			b.WriteString(valueStyle.Render("Press [r] to retry."))
		} else {
			b.WriteString(subtleStyle.Render("Loading bot configuration..."))
		}
		return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
	}
	cfg := m.botConfig
	status := badgeOfflineStyle.Render("STOPPED")
	if cfg.Running {
		status = badgeOnlineStyle.Render("RUNNING")
	}
	b.WriteString(kvStyled("Status", status))
	botToken := "not set"
	if strings.TrimSpace(cfg.BotToken) != "" {
		botToken = "configured"
	}
	b.WriteString(kv("Bot token", botToken))
	chatID := "(not set)"
	if cfg.BotChat != 0 {
		chatID = fmt.Sprintf("%d", cfg.BotChat)
	}
	b.WriteString(kv("Chat ID", chatID))
	controlMode := badgePausedStyle.Render("READ ONLY")
	if strings.TrimSpace(cfg.BotControlToken) != "" {
		controlMode = badgeOnlineStyle.Render("TOKEN SET")
	}
	b.WriteString(kvStyled("Control", controlMode))
	if m.botConfigErr != nil {
		b.WriteString(kvStyled("Freshness", badgeOfflineStyle.Render("STALE")))
		if !m.botLastUpdated.IsZero() {
			b.WriteString(kv("Last update", m.botLastUpdated.Format("15:04:05")))
		}
		message := "Refresh failed: " + friendlyError(m.botConfigErr)
		b.WriteString("\n")
		b.WriteString(badgeOfflineStyle.Render(ansi.Wrap(message, max(panelInnerWidth(calcFullWidth(m.width)), 1), "")))
	}
	if m.botOperation == botOperationFetching {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render(m.spinner.View() + " Refreshing bot state..."))
	}
	b.WriteString("\n")
	if m.botConfigErr != nil {
		b.WriteString(valueStyle.Render("Press [r] to retry before changing bot state."))
	} else if !botReadyToStart(cfg) {
		missing := "bot token and chat ID"
		switch {
		case strings.TrimSpace(cfg.BotToken) != "":
			missing = "chat ID"
		case cfg.BotChat != 0:
			missing = "bot token"
		}
		b.WriteString(subtleStyle.Render("Configure the " + missing + " with [e]."))
	} else if cfg.Running {
		b.WriteString(subtleStyle.Render("[e] edit  [x] stop  [r] refresh"))
	} else {
		b.WriteString(subtleStyle.Render("[e] edit  [s] start  [r] refresh"))
	}
	b.WriteString("\n")
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderBotConfigCompact() string {
	panelWidth := calcFullWidth(m.width)
	innerWidth := panelInnerWidth(panelWidth)
	var b strings.Builder
	b.WriteString(titleStyle.Render("Telegram Bot"))
	if m.botConfig == nil {
		b.WriteString("\n")
		if m.botConfigErr == nil {
			b.WriteString(subtleStyle.Render("Loading configuration..."))
		} else {
			b.WriteString(badgeOfflineStyle.Render("UNAVAILABLE"))
			b.WriteString("\n")
			b.WriteString(badgeOfflineStyle.Render(truncate(friendlyError(m.botConfigErr), innerWidth)))
		}
		return panelStyle.Width(panelWidth).Render(b.String())
	}
	status := badgeOfflineStyle.Render("STOPPED")
	if m.botConfig.Running {
		status = badgeOnlineStyle.Render("RUNNING")
	}
	if m.botConfigErr != nil {
		status += " " + badgeOfflineStyle.Render("STALE")
	}
	token := "not set"
	if m.botConfig.BotToken != "" {
		token = "set"
	}
	chatID := "not set"
	if m.botConfig.BotChat != 0 {
		chatID = fmt.Sprintf("%d", m.botConfig.BotChat)
	}
	b.WriteString("\n")
	if m.botOperation == botOperationFetching {
		status += " " + subtleStyle.Render(m.spinner.View()+" REFRESH")
	}
	b.WriteString(compactKVStyled("Status", status))
	b.WriteString("\n")
	b.WriteString(compactKV("Token", token))
	b.WriteString("\n")
	b.WriteString(compactKV("Chat ID", chatID))
	b.WriteString("\n")
	controlMode := "read only"
	if strings.TrimSpace(m.botConfig.BotControlToken) != "" {
		controlMode = "token set"
	}
	b.WriteString(compactKV("Control", controlMode))
	if m.botConfigErr != nil {
		b.WriteString("\n")
		b.WriteString(badgeOfflineStyle.Render(truncate("Refresh failed: "+friendlyError(m.botConfigErr), innerWidth)))
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
		return subtleStyle.Render("Editor unavailable.")
	}
	e := m.botEditor
	panelWidth := calcFullWidth(m.width)
	inputWidth := max(panelInnerWidth(panelWidth)-19, 1)
	compact := m.width < 50 || m.height < 16
	var b strings.Builder
	b.WriteString(titleStyle.Render("Bot Configuration"))
	b.WriteString("\n")
	b.WriteString(panelDivider(panelWidth, 70))
	b.WriteString("\n")
	fields := []struct{ label, view string }{
		{"Bot token:", e.inputView(0, inputWidth)},
		{"Chat ID:", e.inputView(1, inputWidth)},
		{"Control token:", e.inputView(2, inputWidth)},
	}
	for i, f := range fields {
		prefix := "  "
		if i == e.focus {
			prefix = "> "
		}
		b.WriteString(fmt.Sprintf("%s%-16s %s", prefix, f.label, f.view))
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
		b.WriteString(subtleStyle.Render("Token fields are masked. Blank control token = read-only bot."))
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
	case overlayReload:
		return m.renderConfirm("Reload from Disk", "Reload the daemon configuration from disk?", "Running rules may restart or stop.")
	case overlayDelete:
		name := "selected rule"
		if rule := m.selectedRule(); rule != nil {
			name = safeText(rule.RuleID)
		}
		return m.renderConfirm("Stage Delete", fmt.Sprintf("Stage %s for deletion?", name), "The daemon is unchanged until Apply.")
	case overlayApply:
		return m.renderApplyConfirm()
	case overlayDiscardDraft:
		return m.renderConfirm("Discard Draft", "Discard all local rule changes?", "This cannot be undone in the TUI.")
	case overlayQuitDirty:
		return m.renderConfirm("Quit with Draft", "Discard the local draft and quit?", fmt.Sprintf("%d pending change(s) will be lost.", m.dirtyChangeCount()))
	case overlayCancelEditor:
		return m.renderConfirm("Cancel Editor", "Discard changes in this editor?", "Previously staged changes are kept.")
	case overlayCancelBotEditor:
		return m.renderConfirm("Cancel Bot Editor", "Discard changes in this editor?", "The current bot configuration is unchanged.")
	case overlayBotStop:
		return m.renderConfirm("Stop Bot", "Stop the Telegram bot now?", "Restart it later with [s]. Configuration is unchanged.")
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
		b.WriteString(valueStyle.Render("[Enter/y] confirm    [Esc/n] cancel"))
	}
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
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
	b.WriteString(titleStyle.Render("Apply Draft"))
	b.WriteString("\n\n")
	diff := m.localDiff()
	changes := sortedDiffValues(diff)
	changeLimit := min(len(changes), max(m.height-12, 3))
	for _, line := range changes[:changeLimit] {
		b.WriteString("  " + safeText(line) + "\n")
	}
	if changeLimit < len(changes) {
		b.WriteString(subtleStyle.Render(fmt.Sprintf("  ... %d more change(s)", len(changes)-changeLimit)))
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
	b.WriteString(badgePausedStyle.Render(fmt.Sprintf("Warnings: %d", warnings)))
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render("Changed, disabled, or deleted running rules may close active connections."))
	b.WriteString("\n\n")
	b.WriteString(valueStyle.Render("[Enter/y] apply    [Esc/n] cancel"))
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderApplyConfirmCompact() string {
	changes := sortedDiffValues(m.localDiff())
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
	b.WriteString(titleStyle.Render("Apply Draft"))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%d pending change(s)\n", len(changes)))
	if len(changes) <= 2 {
		for _, line := range changes {
			b.WriteString("  " + truncate(line, max(innerWidth-2, 1)) + "\n")
		}
	} else {
		b.WriteString("  " + truncate(changes[0], max(innerWidth-2, 1)) + "\n")
		b.WriteString(subtleStyle.Render(fmt.Sprintf("  ... %d more change(s)\n", len(changes)-1)))
	}
	b.WriteString(badgePausedStyle.Render(fmt.Sprintf("Warnings: %d", warnings)))
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(truncate("Running connections may close.", innerWidth)))
	b.WriteString("\n")
	b.WriteString(valueStyle.Render("[Enter/y] apply  [Esc/n] cancel"))
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderUDPSettings() string {
	panelWidth := calcFullWidth(m.width)
	var b strings.Builder
	b.WriteString(titleStyle.Render("Global UDP Sessions"))
	b.WriteString("\n\n")
	b.WriteString("Limit: ")
	b.WriteString(responsiveTextInputView(m.udpInput, panelInnerWidth(panelWidth)-7, 32))
	if m.udpInputError != "" {
		b.WriteString("\n")
		b.WriteString(badgeOfflineStyle.Render(m.udpInputError))
	}
	b.WriteString("\n\n")
	b.WriteString(valueStyle.Render("[Enter/Ctrl+S] save draft    [Esc] cancel"))
	return panelStyle.Width(panelWidth).Render(b.String())
}

func (m Model) renderHelp() string {
	shortcuts := []struct{ key, description string }{
		{"q / Ctrl+C", "Quit from main screens"},
		{"Tab", "Switch dashboard and rules"},
		{"Tab / Shift+Tab", "Next / previous editor field"},
		{"r / R", "Refresh / reload disk"},
		{"+ / -", "Refresh interval"},
		{"p", "Pause monitoring"},
		{"up/down j/k", "Move selection or scroll"},
		{"PgUp/PgDn", "Scroll one page"},
		{"Home/End", "Jump to top or bottom"},
		{"enter", "Open rule detail"},
		{"n / e / c", "New / edit / copy rule"},
		{"Space / d", "Stage toggle / delete"},
		{"g", "Global UDP sessions"},
		{"b", "Bot configuration"},
		{"P / A", "Precheck / apply draft"},
		{"u", "Discard draft"},
		{"/ / s", "Filter / sort rules"},
		{"? / F1", "Help (this screen)"},
		{"Esc", "Back or cancel"},
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Keyboard Shortcuts"))
	b.WriteString("\n\n")
	for _, shortcut := range shortcuts {
		b.WriteString(helpShortcutLine(shortcut.key, shortcut.description))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render("Sync "))
	b.WriteString(syncStyle(syncClean).Render("SYNCED") + " ")
	b.WriteString(syncStyle(syncDirty).Render("DRAFT") + " ")
	b.WriteString(syncStyle(syncValidated).Render("VALIDATED") + " ")
	b.WriteString(syncStyle(syncStale).Render("STALE"))
	b.WriteString(subtleStyle.Render("  — edits are local until you press ") + valueStyle.Render("A") + subtleStyle.Render(" (Apply)"))
	return panelStyle.Width(calcFullWidth(m.width)).Render(b.String())
}

func (m Model) renderHelpFooter() string {
	hints := []string{"[j/k]scroll", "[pgup/pgdn]page", "[home/end]jump", "[esc/?/F1]close"}
	return footerStyle.Render(wrapHints(hints, max(m.width, 1)))
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

func (m Model) detailMaxYOffset() int {
	return viewportMaxYOffset(m.renderDetail(), m.mainViewportHeight())
}

func (m *Model) scrollHelp(delta int) {
	limit := m.helpMaxYOffset()
	current := max(0, min(m.overlayYOffset, limit))
	m.overlayYOffset = max(0, min(current+delta, limit))
}

func (m *Model) scrollDetail(delta int) {
	limit := m.detailMaxYOffset()
	current := max(0, min(m.contentYOffset, limit))
	m.contentYOffset = max(0, min(current+delta, limit))
}

func (m *Model) clampScrollOffsets() {
	m.contentYOffset = max(0, min(m.contentYOffset, m.detailMaxYOffset()))
	m.overlayYOffset = max(0, min(m.overlayYOffset, m.helpMaxYOffset()))
}

func panelTitle(name string) string {
	icon := PanelIcons[name]
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
	if value {
		return "yes"
	}
	return "no"
}

func speedLimitStr(value int64) string {
	if value <= 0 {
		return "unlimited"
	}
	return formatRate(float64(value))
}

func maxConnStr(value int) string {
	if value <= 0 {
		return "protocol default"
	}
	return fmt.Sprintf("%d", value)
}

func sourceIPModeLabel(mode engine.SourceIPMode) string {
	mode = engine.SourceIPMode(strings.ToLower(strings.TrimSpace(string(mode))))
	if mode == "" {
		return "off"
	}
	return string(mode)
}

func sourceIPPolicySummary(rule RuleInfo) string {
	mode := sourceIPModeLabel(rule.SourceIPMode)
	switch mode {
	case string(engine.SourceIPModeAllowlist):
		return fmt.Sprintf("ALLOW %d", len(rule.SourceIPs))
	case string(engine.SourceIPModeDenylist):
		return fmt.Sprintf("DENY %d", len(rule.SourceIPs))
	default:
		return "OPEN"
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
