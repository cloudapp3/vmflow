package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ── Color Constants (Tokyo Night) ──────────────────────────────────

var (
	CText   = lipgloss.AdaptiveColor{Light: "#1f2937", Dark: "#c0caf5"}
	CDim    = lipgloss.AdaptiveColor{Light: "#667085", Dark: "#7f849c"}
	CBorder = lipgloss.AdaptiveColor{Light: "#cbd5e1", Dark: "#3b4261"}

	CCyan   = lipgloss.AdaptiveColor{Light: "#0e7490", Dark: "#7dcfff"}
	CGreen  = lipgloss.AdaptiveColor{Light: "#15803d", Dark: "#9ece6a"}
	CYellow = lipgloss.AdaptiveColor{Light: "#a16207", Dark: "#e0af68"}
	CRed    = lipgloss.AdaptiveColor{Light: "#b42318", Dark: "#f7768e"}
	CPurple = lipgloss.AdaptiveColor{Light: "#7e22ce", Dark: "#bb9af7"}
	CBlue   = lipgloss.AdaptiveColor{Light: "#1d4ed8", Dark: "#7aa2f7"}
	COrange = lipgloss.AdaptiveColor{Light: "#c2410c", Dark: "#ff9e64"}
	CTeal   = lipgloss.AdaptiveColor{Light: "#0f766e", Dark: "#73daca"}

	// Upload / Download accent
	CUpload   = lipgloss.AdaptiveColor{Light: "#be185d", Dark: "#ff79c6"}
	CDownload = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#00ff87"}

	// 4-tier threshold
	COk       = lipgloss.AdaptiveColor{Light: "#15803d", Dark: "#00ff87"}
	CWarn     = lipgloss.AdaptiveColor{Light: "#a16207", Dark: "#ffd700"}
	CAlert    = lipgloss.AdaptiveColor{Light: "#c2410c", Dark: "#ffaf5f"}
	CCritical = lipgloss.AdaptiveColor{Light: "#b42318", Dark: "#ff5555"}

	CMuted = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#6c6c6c"}
	CInfo  = lipgloss.AdaptiveColor{Light: "#2563eb", Dark: "#5fafff"}

	// Row background alternating
	CRowA = lipgloss.AdaptiveColor{Light: "#f8fafc", Dark: "#1a1b26"}
	CRowB = lipgloss.AdaptiveColor{Light: "#eef2f7", Dark: "#1f2335"}
	CSel  = lipgloss.AdaptiveColor{Light: "#dbeafe", Dark: "#283457"}
)

// ── Threshold Helpers ──────────────────────────────────────────────

func ThresholdColor(pct float64) lipgloss.TerminalColor {
	switch {
	case pct >= 90:
		return CCritical
	case pct >= 75:
		return CAlert
	case pct >= 50:
		return CWarn
	default:
		return COk
	}
}

// ── Panel Icons ────────────────────────────────────────────────────

var PanelIcons = map[string]string{
	"System":  "◈",
	"Traffic": "◆",
	"Rules":   "☰",
	"Detail":  "◉",
	"Help":    "?",
}

// ── Common Styles ──────────────────────────────────────────────────

var (
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(CBorder).
			Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(CBlue)

	subtleStyle = lipgloss.NewStyle().
			Foreground(CDim)

	valueStyle = lipgloss.NewStyle().
			Foreground(CText)

	labelStyle = lipgloss.NewStyle().
			Foreground(CDim)

	badgeOnlineStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(CGreen)

	badgeOfflineStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(CRed)

	badgePausedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(CYellow)

	statusLineStyle = lipgloss.NewStyle().
			Foreground(CDim)

	footerStyle = lipgloss.NewStyle().
			Foreground(CInfo)

	selectedStyle = lipgloss.NewStyle().
			Background(CSel).
			Foreground(CText).
			Bold(true)

	protoTCPStyle  = lipgloss.NewStyle().Foreground(CCyan)
	protoUDPStyle  = lipgloss.NewStyle().Foreground(CYellow)
	protoBothStyle = lipgloss.NewStyle().Foreground(CPurple)
)

// protocolStyle picks the color style for a protocol string (tcp/udp/tcp+udp).
func protocolStyle(p string) lipgloss.Style {
	switch p {
	case "tcp":
		return protoTCPStyle
	case "udp":
		return protoUDPStyle
	case "tcp+udp":
		return protoBothStyle
	default:
		return valueStyle
	}
}

// protocolLabel renders an upper-cased protocol padded to width for column use.
func protocolLabel(p string) string {
	return protocolStyle(p).Width(7).Render(strings.ToUpper(p))
}

// ── Layout Constants ───────────────────────────────────────────────

const panelGap = 1

type layoutMode int

const (
	layoutCompact layoutMode = iota // < 80
	layoutNarrow                    // 80-99
	layoutMedium                    // 100-139
	layoutWide                      // >= 140
)

func detectLayout(width int) layoutMode {
	switch {
	case width >= 140:
		return layoutWide
	case width >= 100:
		return layoutMedium
	case width >= 80:
		return layoutNarrow
	default:
		return layoutCompact
	}
}

// calcFullWidth returns the width for a full-width panel.
func calcFullWidth(totalW int) int {
	return max(totalW-4, 30)
}

// panelInnerWidth is the usable content width inside panelStyle. Lip Gloss
// includes horizontal padding in Style.Width, so content that uses the full
// style width would wrap by the two padding cells.
func panelInnerWidth(panelWidth int) int {
	return max(panelWidth-2, 1)
}

// calcRowWidths returns (leftW, rightW) for a two-column layout.
func calcRowWidths(totalW int) (int, int) {
	available := max(totalW-4-panelGap, 40)
	left := max(available*40/100, 20)
	right := max(available-left, 20)
	return left, right
}

// Sparkline characters (8 levels).
var sparklineChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
