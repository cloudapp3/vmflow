package tui

import "github.com/charmbracelet/lipgloss"

// ── Color Constants (Tokyo Night) ──────────────────────────────────

var (
	CText   = lipgloss.Color("#c0caf5")
	CDim    = lipgloss.Color("#565f89")
	CBorder = lipgloss.Color("#3b4261")

	CCyan   = lipgloss.Color("#7dcfff")
	CGreen  = lipgloss.Color("#9ece6a")
	CYellow = lipgloss.Color("#e0af68")
	CRed    = lipgloss.Color("#f7768e")
	CPurple = lipgloss.Color("#bb9af7")
	CBlue   = lipgloss.Color("#7aa2f7")
	COrange = lipgloss.Color("#ff9e64")
	CTeal   = lipgloss.Color("#73daca")

	// Upload / Download accent
	CUpload   = lipgloss.Color("#ff79c6")
	CDownload = lipgloss.Color("#00ff87")

	// 4-tier threshold
	COk       = lipgloss.Color("#00ff87")
	CWarn     = lipgloss.Color("#ffd700")
	CAlert    = lipgloss.Color("#ffaf5f")
	CCritical = lipgloss.Color("#ff5555")

	CMuted = lipgloss.Color("#6c6c6c")
	CInfo  = lipgloss.Color("#5fafff")

	// Row background alternating
	CRowA = lipgloss.Color("#1A1B26")
	CRowB = lipgloss.Color("#161623")
	CSel  = lipgloss.Color("#2D4F67")
)

// ── Threshold Helpers ──────────────────────────────────────────────

func ThresholdColor(pct float64) lipgloss.Color {
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
			Foreground(CText)

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
			Foreground(CText)

	protoTCPStyle  = lipgloss.NewStyle().Foreground(CCyan)
	protoUDPStyle  = lipgloss.NewStyle().Foreground(CYellow)
	protoBothStyle = lipgloss.NewStyle().Foreground(CPurple)
)

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

// calcRowWidths returns (leftW, rightW) for a two-column layout.
func calcRowWidths(totalW int) (int, int) {
	available := max(totalW-4-panelGap, 40)
	left := max(available*40/100, 20)
	right := max(available-left, 20)
	return left, right
}

// Sparkline characters (8 levels).
var sparklineChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
