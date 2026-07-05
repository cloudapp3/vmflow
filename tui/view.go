package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ── Main View ──────────────────────────────────────────────────────

func (m Model) View() string {
	if !m.ready {
		return m.spinner.View() + " initializing..."
	}

	if m.showHelp {
		return m.renderHelp()
	}

	if m.confirmReload {
		return m.renderReloadConfirm()
	}

	var content string
	switch m.view {
	case viewDashboard:
		content = m.renderDashboard()
	case viewRules:
		content = m.renderRules()
	case viewDetail:
		content = m.renderDetail()
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	status := m.renderStatusLine()

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		content,
		status,
		footer,
	)
}

// ── Header ─────────────────────────────────────────────────────────

func (m Model) renderHeader() string {
	title := titleStyle.Render("vmflow")
	viewName := subtleStyle.Render(m.viewName())
	badge := m.renderBadge()
	right := subtleStyle.Render(m.spinner.View())
	return lipgloss.JoinHorizontal(lipgloss.Left,
		badge, "  ", title, "  ", viewName,
		strings.Repeat(" ", max(m.width-30, 1)),
		right,
	)
}

func (m Model) viewName() string {
	switch m.view {
	case viewDashboard:
		return "DASHBOARD"
	case viewRules:
		return "RULES"
	case viewDetail:
		return "DETAIL"
	default:
		return ""
	}
}

func (m Model) renderBadge() string {
	if m.health != nil && m.health.OK {
		return badgeOnlineStyle.Render("● ONLINE")
	}
	return badgeOfflineStyle.Render("● OFFLINE")
}

// ── Status Line ────────────────────────────────────────────────────

func (m Model) renderStatusLine() string {
	parts := []string{m.renderBadge()}
	parts = append(parts, subtleStyle.Render("|"))
	parts = append(parts, subtleStyle.Render(m.viewName()))

	if m.view == viewRules {
		parts = append(parts, subtleStyle.Render("|"))
		parts = append(parts, subtleStyle.Render("Sort: "+sortKeyLabel(m.sort)))
	}

	parts = append(parts, subtleStyle.Render("|"))
	interval := fmt.Sprintf("Interval: %ds", int(m.refreshInterval.Seconds()))
	parts = append(parts, subtleStyle.Render(interval))

	if m.paused {
		parts = append(parts, badgePausedStyle.Render("PAUSED"))
	}

	if m.statusText != "" && timeSince(m.statusTime) < 5*time.Second {
		parts = append(parts, subtleStyle.Render("|"), valueStyle.Render(m.statusText))
	}

	return statusLineStyle.Render(strings.Join(parts, " "))
}

// ── Footer ─────────────────────────────────────────────────────────

func (m Model) renderFooter() string {
	hints := []string{
		"[q]uit",
		"[tab]view",
		"[p]ause",
		"[r]efresh",
		"[?]help",
	}
	if m.view == viewRules {
		hints = append(hints, "[↑↓]select [s]ort [/]filter [enter]detail")
	}
	if m.view == viewDetail {
		hints = append(hints, "[esc]back")
	}
	hints = append(hints, "[R]eload")
	return footerStyle.Render(strings.Join(hints, "  "))
}

// ── Dashboard View ─────────────────────────────────────────────────

func (m Model) renderDashboard() string {
	layout := detectLayout(m.width)

	if layout <= layoutCompact {
		return m.renderDashboardCompact()
	}

	sysPanel := m.renderSystemPanel()
	trafficPanel := m.renderTrafficPanel()
	topPanel := m.renderTopRulesPanel()

	var topRow string
	if layout >= layoutMedium {
		leftW, rightW := calcRowWidths(m.width)
		sysP := panelStyle.Width(leftW).Render(sysPanel)
		trafP := panelStyle.Width(rightW).Render(trafficPanel)
		topRow = lipgloss.JoinHorizontal(lipgloss.Top, sysP, trafP)
	} else {
		topRow = panelStyle.Width(calcFullWidth(m.width)).Render(sysPanel)
		topRow += "\n" + panelStyle.Width(calcFullWidth(m.width)).Render(trafficPanel)
	}

	fullW := calcFullWidth(m.width)
	bottomRow := panelStyle.Width(fullW).Render(topPanel)

	return topRow + "\n" + bottomRow
}

func (m Model) renderDashboardCompact() string {
	w := calcFullWidth(m.width)
	traffic := panelStyle.Width(w).Render(m.renderTrafficPanel())
	top := panelStyle.Width(w).Render(m.renderTopRulesPanel())
	return traffic + "\n" + top
}

func (m Model) renderSystemPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("System"))
	b.WriteString("\n")

	status := "OFFLINE"
	if m.health != nil && m.health.OK {
		status = "ONLINE"
	}
	rules := 0
	if m.health != nil {
		rules = m.health.RunningRules
	}

	b.WriteString(kv("Status", status))
	b.WriteString(kv("Rules", fmt.Sprintf("%d running", rules)))
	b.WriteString(kv("Server", m.client.baseURL))

	if m.health != nil && m.health.Time > 0 {
		b.WriteString(kv("Updated", formatTime(m.health.Time)))
	}

	return b.String()
}

func (m Model) renderTrafficPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("Traffic"))
	b.WriteString("\n")

	var totalUp, totalDown int64
	var totalConns int64
	if m.stats != nil {
		for _, s := range m.stats.Items {
			totalUp += s.UploadBytes
			totalDown += s.DownloadBytes
			totalConns += s.Conns
		}
	}

	b.WriteString(kv("Total Upload", formatBytes(totalUp)))
	b.WriteString(kv("Total Download", formatBytes(totalDown)))
	b.WriteString(kv("Active Conns", fmt.Sprintf("%d", totalConns)))

	// Show aggregate rates
	var upRate, downRate float64
	for _, r := range m.rates {
		upRate += r.UploadRate
		downRate += r.DownloadRate
	}
	b.WriteString(kv("Upload Rate", formatRate(upRate)))
	b.WriteString(kv("Download Rate", formatRate(downRate)))

	return b.String()
}

func (m Model) renderTopRulesPanel() string {
	var b strings.Builder
	b.WriteString(panelTitle("Rule Summary"))
	b.WriteString("\n")

	if m.rules == nil || len(m.rules.Items) == 0 {
		b.WriteString(subtleStyle.Render("no rules"))
		return b.String()
	}

	type ruleTraffic struct {
		info RuleInfo
		snap TrafficSnapshot
		rate rateEntry
	}

	statsMap := m.statsMap()
	items := make([]ruleTraffic, 0, len(m.rules.Items))
	for _, r := range m.rules.Items {
		s, _ := statsMap[r.RuleID]
		re := m.rates[r.RuleID]
		items = append(items, ruleTraffic{info: r, snap: s, rate: re})
	}

	sort.Slice(items, func(i, j int) bool {
		return (items[i].snap.UploadBytes + items[i].snap.DownloadBytes) >
			(items[j].snap.UploadBytes + items[j].snap.DownloadBytes)
	})

	limit := min(len(items), 5)
	for i := 0; i < limit; i++ {
		it := items[i]
		proto := protoStyle(it.info.Protocol)
		addr := fmt.Sprintf("%s:%d -> %s:%d",
			it.info.ListenAddr, it.info.ListenPort,
			it.info.TargetAddr, it.info.TargetPort)
		conns := fmt.Sprintf("%d conns", it.snap.Conns)
		upRate := formatRate(it.rate.UploadRate)
		dlRate := formatRate(it.rate.DownloadRate)

		line := fmt.Sprintf("%-16s %s %-34s %8s  ↑%s ↓%s",
			it.info.Name, proto, truncate(addr, 34), conns, upRate, dlRate)
		b.WriteString(valueStyle.Render(line))
		b.WriteString("\n")
	}

	return b.String()
}

// ── Rules View ─────────────────────────────────────────────────────

func (m Model) renderRules() string {
	rules := m.sortedRules()
	if len(rules) == 0 {
		return subtleStyle.Render("No rules found.")
	}

	layout := detectLayout(m.width)
	fullW := calcFullWidth(m.width)

	var b strings.Builder

	// Filter bar
	if m.filterActive {
		b.WriteString(m.filterInput.View())
		b.WriteString("\n")
	} else if m.filterInput.Value() != "" {
		b.WriteString(subtleStyle.Render("filter: " + m.filterInput.Value()))
		b.WriteString("\n")
	}

	// Header
	switch {
	case layout >= layoutWide:
		b.WriteString(fmt.Sprintf("%-16s %-5s %-22s %-22s %6s %10s %10s\n",
			"NAME", "PROTO", "LISTEN", "TARGET", "CONNS", "↑/s", "↓/s"))
	case layout >= layoutMedium:
		b.WriteString(fmt.Sprintf("%-16s %-5s %-18s %-18s %6s %8s %8s\n",
			"NAME", "PROTO", "LISTEN", "TARGET", "CONNS", "↑/s", "↓/s"))
	case layout >= layoutNarrow:
		b.WriteString(fmt.Sprintf("%-16s %-5s %-16s %6s %8s\n",
			"NAME", "PROTO", "LISTEN", "CONNS", "↑/s"))
	default:
		b.WriteString(fmt.Sprintf("%-12s %-4s %6s\n",
			"NAME", "PROTO", "CONNS"))
	}
	b.WriteString(subtleStyle.Render(strings.Repeat("─", min(fullW, m.width-4))))
	b.WriteString("\n")

	statsMap := m.statsMap()

	for i, r := range rules {
		s, _ := statsMap[r.RuleID]
		re := m.rates[r.RuleID]
		selected := i == m.selected

		listen := fmt.Sprintf("%s:%d", r.ListenAddr, r.ListenPort)
		target := fmt.Sprintf("%s:%d", r.TargetAddr, r.TargetPort)
		proto := protoStyle(r.Protocol)

		var line string
		switch {
		case layout >= layoutWide:
			line = fmt.Sprintf("%-16s %s %-22s %-22s %6d %10s %10s",
				r.Name, proto, truncate(listen, 22), truncate(target, 22),
				s.Conns, formatRate(re.UploadRate), formatRate(re.DownloadRate))
		case layout >= layoutMedium:
			line = fmt.Sprintf("%-16s %s %-18s %-18s %6d %8s %8s",
				r.Name, proto, truncate(listen, 18), truncate(target, 18),
				s.Conns, formatRate(re.UploadRate), formatRate(re.DownloadRate))
		case layout >= layoutNarrow:
			line = fmt.Sprintf("%-16s %s %-16s %6d %8s",
				r.Name, proto, truncate(listen, 16),
				s.Conns, formatRate(re.UploadRate))
		default:
			line = fmt.Sprintf("%-12s %s %6d",
				truncate(r.Name, 12), proto, s.Conns)
		}

		if selected {
			prefix := "▶ "
			line = selectedStyle.Render(prefix + line)
		} else {
			bg := CRowA
			if i%2 == 1 {
				bg = CRowB
			}
			line = lipgloss.NewStyle().Background(bg).Render("  " + line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	return panelStyle.Width(fullW).Render(b.String())
}

// ── Detail View ────────────────────────────────────────────────────

func (m Model) renderDetail() string {
	rule := m.selectedRule()
	if rule == nil {
		return subtleStyle.Render("No rule selected.")
	}

	fullW := calcFullWidth(m.width)

	var b strings.Builder
	b.WriteString(titleStyle.Render(rule.Name))
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(strings.Repeat("─", min(fullW-4, 60))))
	b.WriteString("\n")

	b.WriteString(kv("Rule ID", rule.RuleID))
	b.WriteString(kv("Protocol", rule.Protocol))
	b.WriteString(kv("Listen", fmt.Sprintf("%s:%d", rule.ListenAddr, rule.ListenPort)))
	b.WriteString(kv("Target", fmt.Sprintf("%s:%d", rule.TargetAddr, rule.TargetPort)))
	b.WriteString(kv("Enabled", boolStr(rule.Enabled)))
	b.WriteString(kv("Speed Limit", speedLimitStr(rule.SpeedLimit)))
	b.WriteString(kv("Max Conns", maxConnStr(rule.MaxConn)))
	if rule.Remark != "" {
		b.WriteString(kv("Remark", rule.Remark))
	}

	// Traffic stats
	statsMap := m.statsMap()
	s, _ := statsMap[rule.RuleID]
	re := m.rates[rule.RuleID]

	b.WriteString("\n")
	b.WriteString(panelTitle("Traffic"))
	b.WriteString("\n")
	b.WriteString(kv("Upload", fmt.Sprintf("%s  (%s)", formatBytes(s.UploadBytes), formatRate(re.UploadRate))))
	b.WriteString(kv("Download", fmt.Sprintf("%s  (%s)", formatBytes(s.DownloadBytes), formatRate(re.DownloadRate))))
	b.WriteString(kv("Connections", fmt.Sprintf("%d", s.Conns)))

	// Sparkline
	if h, ok := m.history[rule.RuleID]; ok && len(h.uploadRates) > 1 {
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("Upload Rate"))
		b.WriteString("\n")
		b.WriteString(renderSparkline(h.uploadRates, CUpload, fullW-6))
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("Download Rate"))
		b.WriteString("\n")
		b.WriteString(renderSparkline(h.downloadRates, CDownload, fullW-6))
	}

	return panelStyle.Width(fullW).Render(b.String())
}

// ── Help View ──────────────────────────────────────────────────────

func (m Model) renderHelp() string {
	fullW := calcFullWidth(m.width)

	var b strings.Builder
	b.WriteString(titleStyle.Render("Keyboard Shortcuts"))
	b.WriteString("\n\n")

	shortcuts := []struct{ key, desc string }{
		{"q / Ctrl+C", "Quit"},
		{"tab", "Switch view (Dashboard → Rules → Detail)"},
		{"p", "Pause / resume auto-refresh"},
		{"r", "Force refresh"},
		{"+ / -", "Adjust refresh interval (1-10s)"},
		{"R", "Reload configuration"},
		{"?", "Toggle this help"},
		{"", ""},
		{"↑ / ↓", "Navigate rule list"},
		{"s", "Cycle sort order"},
		{"/", "Filter rules"},
		{"enter", "View rule detail"},
		{"esc", "Back / cancel"},
	}

	for _, s := range shortcuts {
		if s.key == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(fmt.Sprintf("  %-14s %s\n",
			valueStyle.Render(s.key), subtleStyle.Render(s.desc)))
	}

	return panelStyle.Width(fullW).Render(b.String())
}

// ── Reload Confirmation ────────────────────────────────────────────

func (m Model) renderReloadConfirm() string {
	fullW := calcFullWidth(m.width)

	var b strings.Builder
	b.WriteString(titleStyle.Render("Confirm Reload"))
	b.WriteString("\n\n")
	b.WriteString("Reload configuration from disk and apply snapshot?")
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render("This will restart/stop/start rules as needed."))
	b.WriteString("\n\n")
	b.WriteString(valueStyle.Render("[Enter/y] confirm    [Esc/n] cancel"))

	return panelStyle.Width(fullW).Render(b.String())
}

// ── Render Helpers ─────────────────────────────────────────────────

func panelTitle(name string) string {
	icon := PanelIcons[name]
	return fmt.Sprintf("%s %s", icon, titleStyle.Render(name))
}

func kv(key, value string) string {
	return fmt.Sprintf("%s%-14s %s\n", "  ", labelStyle.Render(key+":"), valueStyle.Render(value))
}

func protoStyle(proto string) string {
	p := strings.ToUpper(proto)
	switch proto {
	case "tcp":
		return protoTCPStyle.Render(p)
	case "udp":
		return protoUDPStyle.Render(p)
	case "tcp+udp":
		return protoBothStyle.Render(p)
	default:
		return p
	}
}

func renderSparkline(data []float64, color lipgloss.Color, width int) string {
	if len(data) == 0 {
		return ""
	}

	maxVal := 0.0
	for _, v := range data {
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == 0 {
		maxVal = 1
	}

	// Take last `width` samples
	start := 0
	if len(data) > width {
		start = len(data) - width
	}
	visible := data[start:]

	var b strings.Builder
	style := lipgloss.NewStyle().Foreground(color)
	for _, v := range visible {
		level := int(math.Round(v / maxVal * float64(len(sparklineChars)-1)))
		level = max(0, min(level, len(sparklineChars)-1))
		b.WriteString(style.Render(string(sparklineChars[level])))
	}
	return b.String()
}

// ── Format Helpers ─────────────────────────────────────────────────

func formatBytes(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case n >= TB:
		return fmt.Sprintf("%.1fT", float64(n)/float64(TB))
	case n >= GB:
		return fmt.Sprintf("%.1fG", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1fM", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1fK", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func formatRate(bytesPerSec float64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytesPerSec >= GB:
		return fmt.Sprintf("%.1fG/s", bytesPerSec/GB)
	case bytesPerSec >= MB:
		return fmt.Sprintf("%.1fM/s", bytesPerSec/MB)
	case bytesPerSec >= KB:
		return fmt.Sprintf("%.1fK/s", bytesPerSec/KB)
	default:
		return fmt.Sprintf("%.0fB/s", bytesPerSec)
	}
}

func formatTime(unix int64) string {
	return fmt.Sprintf("%d:%02d:%02d",
		(unix/3600)%24, (unix/60)%60, unix%60)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-2] + ".."
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func speedLimitStr(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return formatRate(float64(n))
}

func maxConnStr(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}

func timeSince(t time.Time) time.Duration {
	return time.Since(t)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
