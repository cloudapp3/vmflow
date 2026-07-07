package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// ── View & Sort Types ──────────────────────────────────────────────

type viewMode int

const (
	viewDashboard viewMode = iota
	viewRules
	viewDetail
)

type sortKey int

const (
	sortName sortKey = iota
	sortConns
	sortUpload
	sortDownload
)

var sortKeys = []sortKey{sortName, sortConns, sortUpload, sortDownload}

func sortKeyLabel(s sortKey) string {
	switch s {
	case sortName:
		return "name"
	case sortConns:
		return "conns"
	case sortUpload:
		return "upload"
	case sortDownload:
		return "download"
	default:
		return "?"
	}
}

// ── Messages ───────────────────────────────────────────────────────

type tickMsg time.Time

type healthMsg struct {
	resp *HealthResponse
	err  error
}

type statsMsg struct {
	resp *StatsResponse
	err  error
}

type rulesMsg struct {
	resp *RulesResponse
	err  error
}

type reloadMsg struct {
	resp *ReloadResponse
	err  error
}

// ── Rate Tracking ──────────────────────────────────────────────────

type rateEntry struct {
	UploadRate   float64 // bytes/sec
	DownloadRate float64 // bytes/sec
}

type rateHistory struct {
	uploadRates   []float64
	downloadRates []float64
}

const historySize = 60

func (rh *rateHistory) push(up, down float64) {
	rh.uploadRates = append(rh.uploadRates, up)
	rh.downloadRates = append(rh.downloadRates, down)
	if len(rh.uploadRates) > historySize {
		rh.uploadRates = rh.uploadRates[1:]
		rh.downloadRates = rh.downloadRates[1:]
	}
}

// ── Model ──────────────────────────────────────────────────────────

type Model struct {
	ctx    context.Context
	client *Client

	// Data
	health    *HealthResponse
	stats     *StatsResponse
	prevStats *StatsResponse
	rules     *RulesResponse
	rates     map[string]rateEntry
	history   map[string]*rateHistory

	// UI state
	view          viewMode
	selected      int
	sort          sortKey
	sortDesc      bool
	paused        bool
	showHelp      bool
	filterActive  bool
	filterInput   textinput.Model
	confirmReload bool
	statusText    string
	statusTime    time.Time

	// Layout
	width  int
	height int

	refreshInterval time.Duration
	spinner         spinner.Model
	viewport        viewport.Model
	ready           bool
}

// ── Public Entry ───────────────────────────────────────────────────

func Run(ctx context.Context, stdout io.Writer, addr, token string, httpClient *http.Client, headers http.Header) error {
	m := newModel(ctx, addr, token)
	m.client.SetHTTPClient(httpClient)
	m.client.SetHeaders(headers)
	p := tea.NewProgram(m,
		tea.WithOutput(stdout),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}

func newModel(ctx context.Context, addr string, token ...string) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = subtleStyle

	ti := textinput.New()
	ti.Placeholder = "filter by name..."
	ti.CharLimit = 40

	return Model{
		ctx:             ctx,
		client:          NewClient(addr, token...),
		rates:           make(map[string]rateEntry),
		history:         make(map[string]*rateHistory),
		view:            viewDashboard,
		sort:            sortName,
		refreshInterval: 3 * time.Second,
		spinner:         s,
		filterInput:     ti,
	}
}

// ── Bubbletea Interface ────────────────────────────────────────────

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		fetchHealthCmd(m.ctx, m.client),
		fetchStatsCmd(m.ctx, m.client),
		fetchRulesCmd(m.ctx, m.client),
		tickCmd(m.refreshInterval),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.paused {
			return m, tickCmd(m.refreshInterval)
		}
		return m, tea.Batch(
			fetchHealthCmd(m.ctx, m.client),
			fetchStatsCmd(m.ctx, m.client),
			fetchRulesCmd(m.ctx, m.client),
			tickCmd(m.refreshInterval),
		)

	case healthMsg:
		if msg.err != nil {
			m.health = nil
			m.statusText = fmt.Sprintf("connect error: %v", msg.err)
			m.statusTime = time.Now()
		} else {
			m.health = msg.resp
		}
		return m, nil

	case statsMsg:
		if msg.err == nil {
			m.computeRates(msg.resp)
			m.prevStats = m.stats
			m.stats = msg.resp
		}
		return m, nil

	case rulesMsg:
		if msg.err == nil {
			m.rules = msg.resp
		}
		return m, nil

	case reloadMsg:
		if msg.err != nil {
			m.statusText = fmt.Sprintf("reload failed: %v", msg.err)
		} else {
			m.statusText = fmt.Sprintf("reload ok: %d rules applied", msg.resp.RuleCount)
		}
		m.statusTime = time.Now()
		m.confirmReload = false
		return m, tea.Batch(
			fetchHealthCmd(m.ctx, m.client),
			fetchStatsCmd(m.ctx, m.client),
			fetchRulesCmd(m.ctx, m.client),
		)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-4)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - 4
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

// ── Key Handling ───────────────────────────────────────────────────

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirmReload {
		return m.handleReloadConfirm(msg)
	}

	if m.filterActive {
		return m.handleFilterInput(msg)
	}

	if m.showHelp {
		if msg.String() == "?" || msg.String() == "esc" {
			m.showHelp = false
		}
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = !m.showHelp
	case "tab":
		m.view = (m.view + 1) % 3
		m.selected = 0
	case "p":
		m.paused = !m.paused
		if m.paused {
			m.statusText = "paused"
		} else {
			m.statusText = "resumed"
		}
		m.statusTime = time.Now()
	case "r":
		return m, tea.Batch(
			fetchHealthCmd(m.ctx, m.client),
			fetchStatsCmd(m.ctx, m.client),
			fetchRulesCmd(m.ctx, m.client),
		)
	case "+", "=":
		if m.refreshInterval > time.Second {
			m.refreshInterval -= time.Second
			m.statusText = fmt.Sprintf("interval: %ds", int(m.refreshInterval.Seconds()))
			m.statusTime = time.Now()
		}
	case "-":
		if m.refreshInterval < 10*time.Second {
			m.refreshInterval += time.Second
			m.statusText = fmt.Sprintf("interval: %ds", int(m.refreshInterval.Seconds()))
			m.statusTime = time.Now()
		}
	case "R":
		m.confirmReload = true
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		rules := m.filteredRules()
		if m.selected < len(rules)-1 {
			m.selected++
		}
	case "enter":
		rules := m.filteredRules()
		if m.selected < len(rules) {
			m.view = viewDetail
		}
	case "s":
		for i, sk := range sortKeys {
			if sk == m.sort {
				m.sort = sortKeys[(i+1)%len(sortKeys)]
				break
			}
		}
		m.sortDesc = !m.sortDesc
	case "/":
		m.filterActive = true
		m.filterInput.Focus()
	}

	return m, nil
}

func (m Model) handleReloadConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "y":
		return m, reloadCmd(m.ctx, m.client)
	case "esc", "n":
		m.confirmReload = false
	}
	return m, nil
}

func (m Model) handleFilterInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		return m, nil
	case "enter":
		m.filterActive = false
		m.filterInput.Blur()
		return m, nil
	default:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}
}

// ── Mouse Handling ─────────────────────────────────────────────────

func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.MouseWheelUp:
		if m.selected > 0 {
			m.selected--
		}
	case tea.MouseWheelDown:
		rules := m.filteredRules()
		if m.selected < len(rules)-1 {
			m.selected++
		}
	}
	return m, nil
}

// ── Rate Computation ───────────────────────────────────────────────

func (m *Model) computeRates(newStats *StatsResponse) {
	if m.stats == nil {
		return
	}

	prevMap := make(map[string]TrafficSnapshot, len(m.stats.Items))
	for _, s := range m.stats.Items {
		prevMap[s.RuleID] = s
	}

	for _, cur := range newStats.Items {
		prev, ok := prevMap[cur.RuleID]
		if !ok {
			continue
		}
		dt := float64(cur.UpdatedTime - prev.UpdatedTime)
		if dt <= 0 {
			continue
		}
		re := rateEntry{
			UploadRate:   float64(cur.UploadBytes-prev.UploadBytes) / dt,
			DownloadRate: float64(cur.DownloadBytes-prev.DownloadBytes) / dt,
		}
		m.rates[cur.RuleID] = re

		h, exists := m.history[cur.RuleID]
		if !exists {
			h = &rateHistory{}
			m.history[cur.RuleID] = h
		}
		h.push(re.UploadRate, re.DownloadRate)
	}
}

// ── Sorting & Filtering ───────────────────────────────────────────

func (m Model) filteredRules() []RuleInfo {
	if m.rules == nil {
		return nil
	}
	filter := strings.ToLower(m.filterInput.Value())
	result := make([]RuleInfo, 0, len(m.rules.Items))
	for _, r := range m.rules.Items {
		if filter != "" {
			if !strings.Contains(strings.ToLower(r.Name), filter) &&
				!strings.Contains(strings.ToLower(r.RuleID), filter) {
				continue
			}
		}
		result = append(result, r)
	}
	return result
}

func (m Model) sortedRules() []RuleInfo {
	rules := m.filteredRules()
	statsMap := m.statsMap()

	sort.SliceStable(rules, func(i, j int) bool {
		var less bool
		si, _ := statsMap[rules[i].RuleID]
		sj, _ := statsMap[rules[j].RuleID]

		switch m.sort {
		case sortConns:
			less = si.Conns < sj.Conns
		case sortUpload:
			less = si.UploadBytes < sj.UploadBytes
		case sortDownload:
			less = si.DownloadBytes < sj.DownloadBytes
		default: // sortName
			less = rules[i].Name < rules[j].Name
		}

		if m.sortDesc {
			return !less
		}
		return less
	})
	return rules
}

func (m Model) statsMap() map[string]TrafficSnapshot {
	result := make(map[string]TrafficSnapshot)
	if m.stats != nil {
		for _, s := range m.stats.Items {
			result[s.RuleID] = s
		}
	}
	return result
}

func (m Model) selectedRule() *RuleInfo {
	rules := m.sortedRules()
	if m.selected >= 0 && m.selected < len(rules) {
		return &rules[m.selected]
	}
	return nil
}

// ── Commands ───────────────────────────────────────────────────────

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func fetchHealthCmd(ctx context.Context, c *Client) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.Health(ctx)
		return healthMsg{resp: resp, err: err}
	}
}

func fetchStatsCmd(ctx context.Context, c *Client) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.Stats(ctx)
		return statsMsg{resp: resp, err: err}
	}
}

func fetchRulesCmd(ctx context.Context, c *Client) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.Rules(ctx)
		return rulesMsg{resp: resp, err: err}
	}
}

func reloadCmd(ctx context.Context, c *Client) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.Reload(ctx)
		return reloadMsg{resp: resp, err: err}
	}
}
