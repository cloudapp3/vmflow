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

type viewMode int

const (
	viewDashboard viewMode = iota
	viewRules
	viewDetail
	viewEditor
	viewPrecheck
	viewApplyResult
	viewBotConfig
	viewBotEditor
)

type overlayMode int

const (
	overlayNone overlayMode = iota
	overlayHelp
	overlayReload
	overlayDelete
	overlayApply
	overlayDiscardDraft
	overlayQuitDirty
	overlayCancelEditor
	overlayUDPSettings
	overlayBotStop
	overlayCancelBotEditor
)

type operationState int

const (
	operationIdle operationState = iota
	operationPrechecking
	operationApplying
	operationReloading
)

type botOperationState int

const (
	botOperationIdle botOperationState = iota
	botOperationFetching
	botOperationSaving
	botOperationStarting
	botOperationStopping
)

type syncState int

const (
	syncUnavailable syncState = iota
	syncClean
	syncDirty
	syncValidated
	syncStale
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

type tickMsg time.Time

type statsMsg struct {
	requestID int64
	resp      *StatsResponse
	err       error
}

type rulesMsg struct {
	requestID int64
	resp      *RulesResponse
	err       error
}

type sessionMsg struct {
	requestID int64
	resp      *SessionResponse
	err       error
}

type configRulesMsg struct {
	requestID int64
	resp      *ConfigRulesResponse
	err       error
}

type botConfigMsg struct {
	requestID int64
	operation botOperationState
	resp      *BotConfigResponse
	err       error
}

type botActionMsg struct {
	requestID int64
	operation botOperationState
	action    string
	err       error
}

type precheckMsg struct {
	resp *PrecheckResponse
	err  error
}

type applyMsg struct {
	resp *ApplyResponse
	err  error
}

type reloadMsg struct {
	resp *ReloadResponse
	err  error
}

type rateEntry struct {
	UploadRate   float64
	DownloadRate float64
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

type draftConfig struct {
	BaseRevision   string
	BaseETag       string
	UDPMaxSessions int
	Rules          []RuleInfo
	Deleted        map[string]RuleInfo
}

func (d *draftConfig) request() ConfigRulesRequest {
	if d == nil {
		return ConfigRulesRequest{}
	}
	return ConfigRulesRequest{UDPMaxSessions: d.UDPMaxSessions, Rules: cloneRules(d.Rules)}
}

type Model struct {
	ctx    context.Context
	client *Client

	// Runtime monitoring data.
	stats       *StatsResponse
	prevStats   *StatsResponse
	rules       *RulesResponse
	rates       map[string]rateEntry
	history     map[string]*rateHistory
	lastStatsAt time.Time

	// Configuration management data.
	session            *SessionResponse
	config             *ConfigRulesResponse
	draft              *draftConfig
	precheckResult     *PrecheckResponse
	applyResult        *ApplyResponse
	botConfig          *BotConfigResponse
	botEditor          *botConfigEditor
	botConfigErr       error
	botLastUpdated     time.Time
	botRebasePending   bool
	botReconcileAction string
	editor             *ruleEditor
	transientDraft     bool
	remoteRevision     string
	configErr          error
	sessionErr         error
	connectionErr      error
	rulesErr           error
	sync               syncState
	operation          operationState
	botOperation       botOperationState
	overlay            overlayMode
	resultSelected     int

	// List and input state.
	view           viewMode
	selectedRuleID string
	sort           sortKey
	sortDesc       bool
	paused         bool
	filterActive   bool
	filterInput    textinput.Model
	udpInput       textinput.Model
	udpInputError  string
	statusText     string
	statusTime     time.Time
	contentYOffset int
	overlayYOffset int

	// Request generations prevent slower responses from replacing newer state.
	statsRequestID       int64
	rulesRequestID       int64
	sessionRequestID     int64
	configRequestID      int64
	lastConfigResponseID int64
	configBarrierID      int64
	botRequestID         int64

	width  int
	height int

	refreshInterval time.Duration
	spinner         spinner.Model
	viewport        viewport.Model
	ready           bool
}

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

	filter := textinput.New()
	filter.Placeholder = "filter by name or id..."
	filter.CharLimit = 80

	udpInput := textinput.New()
	udpInput.Prompt = ""
	udpInput.Placeholder = "global UDP sessions"
	udpInput.CharLimit = 5
	udpInput.Width = 16

	return Model{
		ctx:              ctx,
		client:           NewClient(addr, token...),
		rates:            make(map[string]rateEntry),
		history:          make(map[string]*rateHistory),
		view:             viewDashboard,
		sort:             sortName,
		sync:             syncUnavailable,
		refreshInterval:  3 * time.Second,
		spinner:          s,
		filterInput:      filter,
		udpInput:         udpInput,
		statsRequestID:   1,
		rulesRequestID:   1,
		sessionRequestID: 1,
		configRequestID:  1,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		fetchStatsCmd(m.ctx, m.client, m.statsRequestID),
		fetchRulesCmd(m.ctx, m.client, m.rulesRequestID),
		fetchSessionCmd(m.ctx, m.client, m.sessionRequestID),
		fetchConfigRulesCmd(m.ctx, m.client, m.configRequestID),
		tickCmd(m.refreshInterval),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.paused {
			return m, tickCmd(m.refreshInterval)
		}
		statsRequestID := m.beginStatsRequest()
		rulesRequestID := m.beginRulesRequest()
		m.configRequestID++
		commands := []tea.Cmd{
			fetchStatsCmd(m.ctx, m.client, statsRequestID),
			fetchRulesCmd(m.ctx, m.client, rulesRequestID),
			fetchConfigRulesCmd(m.ctx, m.client, m.configRequestID),
		}
		if m.view == viewBotConfig && m.botOperation == botOperationIdle {
			botRequestID := m.beginBotOperation(botOperationFetching)
			commands = append(commands, fetchBotConfigCmd(m.ctx, m.client, botRequestID))
		}
		commands = append(commands, tickCmd(m.refreshInterval))
		return m, tea.Batch(commands...)

	case statsMsg:
		if msg.requestID != m.statsRequestID {
			return m, nil
		}
		if msg.err != nil {
			m.connectionErr = msg.err
		} else {
			m.connectionErr = nil
			sampledAt := time.Now()
			m.computeRates(msg.resp, sampledAt)
			m.prevStats = m.stats
			m.stats = msg.resp
			m.lastStatsAt = sampledAt
		}
		return m, nil

	case rulesMsg:
		if msg.requestID != m.rulesRequestID {
			return m, nil
		}
		if msg.err != nil {
			m.rulesErr = msg.err
		} else {
			m.rules = msg.resp
			m.rulesErr = nil
		}
		return m, nil

	case sessionMsg:
		if msg.requestID != m.sessionRequestID {
			return m, nil
		}
		if msg.err != nil {
			m.sessionErr = msg.err
		} else {
			m.session = msg.resp
			m.sessionErr = nil
		}
		return m, nil

	case configRulesMsg:
		if msg.requestID < m.lastConfigResponseID {
			return m, nil
		}
		m.lastConfigResponseID = msg.requestID
		if msg.err != nil {
			m.configErr = msg.err
			if m.config == nil {
				m.sync = syncUnavailable
			}
			return m, nil
		}
		if m.configBarrierID != 0 && msg.requestID >= m.configBarrierID {
			m.configBarrierID = 0
		}
		m.configErr = nil
		m.acceptConfig(msg.resp)
		return m, nil

	case botConfigMsg:
		if msg.requestID != m.botRequestID || msg.operation != m.botOperation {
			return m, nil
		}
		m.botOperation = botOperationIdle
		if msg.err != nil {
			if msg.operation == botOperationFetching {
				m.botConfigErr = msg.err
				message := "bot config unavailable: " + friendlyError(msg.err)
				if m.botReconcileAction != "" {
					message = "could not reconcile bot " + m.botReconcileAction + ": " + friendlyError(msg.err)
				}
				if m.botRebasePending && m.botEditor != nil {
					message = "could not load the latest bot config: " + friendlyError(msg.err)
					m.botEditor.formError = message + "; press Ctrl+R to retry"
				}
				m.setStatus(message)
				return m, nil
			}
			switch apiStatus(msg.err) {
			case http.StatusPreconditionFailed:
				m.botRebasePending = true
				if m.botEditor != nil {
					m.botEditor.formError = "Bot configuration changed remotely; loading the latest version..."
				}
				m.setStatus("bot config changed remotely; loading the latest version")
				botRequestID := m.beginBotOperation(botOperationFetching)
				configRequestID := m.beginConfigBarrier()
				return m, tea.Batch(
					fetchBotConfigCmd(m.ctx, m.client, botRequestID),
					fetchConfigRulesCmd(m.ctx, m.client, configRequestID),
				)
			case http.StatusForbidden:
				message := "bot config requires admin"
				if m.botEditor != nil {
					m.botEditor.formError = message
				}
				m.setStatus(message)
			case http.StatusUnprocessableEntity:
				message := "bot config rejected: invalid Telegram token or chat settings"
				if m.botEditor != nil {
					m.botEditor.formError = message
				}
				m.setStatus(message)
			case http.StatusServiceUnavailable:
				message, refresh, committed := botConfigUnavailableState(msg.err)
				m.setStatus(message)
				if m.botEditor != nil {
					m.botEditor.formError = message
				}
				if refresh {
					if committed {
						m.botEditor = nil
						m.view = viewBotConfig
						m.botRebasePending = false
					} else if m.botEditor != nil {
						m.botRebasePending = true
					}
					botRequestID := m.beginBotOperation(botOperationFetching)
					configRequestID := m.beginConfigBarrier()
					return m, tea.Batch(
						fetchBotConfigCmd(m.ctx, m.client, botRequestID),
						fetchConfigRulesCmd(m.ctx, m.client, configRequestID),
					)
				}
			default:
				message := "bot config result is uncertain; refreshing: " + friendlyError(msg.err)
				m.setStatus(message)
				if m.botEditor != nil {
					m.botEditor.formError = message
					m.botRebasePending = true
				}
				botRequestID := m.beginBotOperation(botOperationFetching)
				configRequestID := m.beginConfigBarrier()
				return m, tea.Batch(
					fetchBotConfigCmd(m.ctx, m.client, botRequestID),
					fetchConfigRulesCmd(m.ctx, m.client, configRequestID),
				)
			}
			return m, nil
		}
		m.botConfig = msg.resp
		m.botConfigErr = nil
		m.botLastUpdated = time.Now()
		if msg.operation == botOperationFetching && m.botRebasePending && m.botEditor != nil {
			m.botEditor.rebase(msg.resp)
			m.botEditor.formError = "Latest remote configuration loaded. Review and save again."
			m.botRebasePending = false
			m.setStatus("latest bot config loaded; review and save again")
			return m, nil
		}
		if msg.operation == botOperationFetching {
			m.botRebasePending = false
			if m.botReconcileAction != "" {
				state := "STOPPED"
				if msg.resp != nil && msg.resp.Running {
					state = "RUNNING"
				}
				m.setStatus("bot " + m.botReconcileAction + " result reconciled: " + state)
				m.botReconcileAction = ""
			}
		}
		if msg.operation == botOperationSaving && m.view == viewBotEditor {
			m.botEditor = nil
			m.view = viewBotConfig
			m.botRebasePending = false
			m.setStatus("bot config saved")
			configRequestID := m.beginConfigBarrier()
			return m, fetchConfigRulesCmd(m.ctx, m.client, configRequestID)
		}
		return m, nil

	case botActionMsg:
		if msg.requestID != m.botRequestID || msg.operation != m.botOperation {
			return m, nil
		}
		m.botOperation = botOperationIdle
		if msg.err != nil {
			m.botReconcileAction = msg.action
			m.setStatus("bot " + msg.action + " failed: " + botControlErrorMessage(msg.action, msg.err) + "; refreshing state")
			requestID := m.beginBotOperation(botOperationFetching)
			return m, fetchBotConfigCmd(m.ctx, m.client, requestID)
		}
		m.botReconcileAction = ""
		m.setStatus("bot " + msg.action + " ok")
		requestID := m.beginBotOperation(botOperationFetching)
		return m, fetchBotConfigCmd(m.ctx, m.client, requestID)

	case precheckMsg:
		return m.handlePrecheckResult(msg)

	case applyMsg:
		return m.handleApplyResult(msg)

	case reloadMsg:
		m.operation = operationIdle
		m.overlay = overlayNone
		if msg.err != nil {
			m.setStatus("reload failed: " + controlErrorMessage(msg.err))
			return m, nil
		}
		m.setStatus(fmt.Sprintf("reload ok: %d rules applied", msg.resp.RuleCount))
		statsRequestID := m.beginStatsRequest()
		rulesRequestID := m.beginRulesRequest()
		m.configRequestID++
		return m, tea.Batch(
			fetchStatsCmd(m.ctx, m.client, statsRequestID),
			fetchRulesCmd(m.ctx, m.client, rulesRequestID),
			fetchConfigRulesCmd(m.ctx, m.client, m.configRequestID),
		)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(msg.Width, max(msg.Height-3, 1))
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = max(msg.Height-3, 1)
		}
		m.clampScrollOffsets()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func botControlErrorMessage(action string, err error) string {
	switch apiStatus(err) {
	case http.StatusForbidden:
		return "admin role required"
	case http.StatusUnprocessableEntity:
		return "invalid Telegram token or chat settings"
	case http.StatusServiceUnavailable:
		message := strings.ToLower(err.Error())
		if action == "start" && strings.Contains(message, "readiness") {
			return "Telegram readiness check failed; the previous bot is unchanged"
		}
		if strings.Contains(message, "controller unavailable") {
			return "bot controller unavailable; state uncertain"
		}
		return "bot lifecycle operation failed; state uncertain"
	case 0:
		return friendlyError(err)
	default:
		return err.Error()
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if updated, cmd, handled := m.handlePendingOperationKey(msg); handled {
		return updated, cmd
	}
	if m.overlay != overlayNone {
		return m.handleOverlayKey(msg)
	}
	if m.view == viewEditor {
		return m.handleEditorKey(msg)
	}
	if m.view == viewBotEditor {
		return m.handleBotEditorKey(msg)
	}
	if m.view == viewBotConfig {
		return m.handleBotConfigKey(msg)
	}
	if m.filterActive {
		return m.handleFilterInput(msg)
	}
	if m.view == viewPrecheck {
		return m.handlePrecheckKey(msg)
	}
	if m.view == viewApplyResult {
		switch msg.String() {
		case "esc", "enter":
			m.view = viewRules
			return m, nil
		case "up", "k":
			m.resultSelected = max(0, m.resultSelected-1)
			return m, nil
		case "down", "j":
			itemCount := 0
			if m.applyResult != nil {
				itemCount = len(m.applyResult.Result.Items)
			}
			m.resultSelected = min(max(itemCount-1, 0), m.resultSelected+1)
			return m, nil
		case "pgup":
			m.resultSelected = max(0, m.resultSelected-m.applyResultPageSize())
			return m, nil
		case "pgdown":
			itemCount := 0
			if m.applyResult != nil {
				itemCount = len(m.applyResult.Result.Items)
			}
			m.resultSelected = min(max(itemCount-1, 0), m.resultSelected+m.applyResultPageSize())
			return m, nil
		case "home":
			m.resultSelected = 0
			return m, nil
		case "end":
			itemCount := 0
			if m.applyResult != nil {
				itemCount = len(m.applyResult.Result.Items)
			}
			m.resultSelected = max(itemCount-1, 0)
			return m, nil
		}
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
		if m.view == viewDetail {
			m.view = viewRules
			m.contentYOffset = 0
		}
	case "tab":
		if m.view == viewDashboard {
			m.view = viewRules
		} else {
			m.view = viewDashboard
		}
		m.contentYOffset = 0
	case "p":
		m.paused = !m.paused
		if m.paused {
			m.setStatus("monitoring paused")
		} else {
			m.setStatus("monitoring resumed")
		}
	case "r":
		return m.refreshNow()
	case "+", "=":
		if m.refreshInterval > time.Second {
			m.refreshInterval -= time.Second
			m.setStatus(fmt.Sprintf("interval: %ds", int(m.refreshInterval.Seconds())))
		}
	case "-":
		if m.refreshInterval < 10*time.Second {
			m.refreshInterval += time.Second
			m.setStatus(fmt.Sprintf("interval: %ds", int(m.refreshInterval.Seconds())))
		}
	case "R":
		if m.draft != nil {
			m.setStatus("discard the local draft before reloading from disk")
		} else if !m.canReload() {
			m.setStatus("reload requires an admin session")
		} else {
			m.overlay = overlayReload
		}
	case "b":
		return m.openBotConfig()
	case "up", "k":
		if m.view == viewDetail {
			m.scrollDetail(-1)
		} else if m.view == viewRules {
			m.moveSelection(-1)
		}
	case "down", "j":
		if m.view == viewDetail {
			m.scrollDetail(1)
		} else if m.view == viewRules {
			m.moveSelection(1)
		}
	case "pgup":
		if m.view == viewDetail {
			m.scrollDetail(-m.mainViewportHeight())
		} else if m.view == viewRules {
			m.moveSelection(-m.rulePageSize())
		}
	case "pgdown":
		if m.view == viewDetail {
			m.scrollDetail(m.mainViewportHeight())
		} else if m.view == viewRules {
			m.moveSelection(m.rulePageSize())
		}
	case "home":
		if m.view == viewDetail {
			m.contentYOffset = 0
		} else if m.view == viewRules {
			m.selectRuleAt(0)
		}
	case "end":
		if m.view == viewDetail {
			m.contentYOffset = m.detailMaxYOffset()
		} else if m.view == viewRules {
			m.selectRuleAt(len(m.sortedRules()) - 1)
		}
	case "enter":
		if m.view == viewRules && m.selectedRule() != nil {
			m.view = viewDetail
			m.contentYOffset = 0
		}
	case "s":
		if m.view == viewRules {
			m.cycleSort()
		}
	case "/":
		if m.view == viewRules {
			m.filterActive = true
			m.filterInput.Focus()
		}
	default:
		if m.view == viewRules || m.view == viewDetail {
			return m.handleManagementKey(msg)
		}
	}
	return m, nil
}

func (m Model) refreshNow() (tea.Model, tea.Cmd) {
	statsRequestID := m.beginStatsRequest()
	rulesRequestID := m.beginRulesRequest()
	sessionRequestID := m.beginSessionRequest()
	m.configRequestID++
	return m, tea.Batch(
		fetchStatsCmd(m.ctx, m.client, statsRequestID),
		fetchRulesCmd(m.ctx, m.client, rulesRequestID),
		fetchSessionCmd(m.ctx, m.client, sessionRequestID),
		fetchConfigRulesCmd(m.ctx, m.client, m.configRequestID),
	)
}

func (m Model) handleFilterInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.reconcileSelection()
		return m, nil
	case "enter":
		m.filterActive = false
		m.filterInput.Blur()
		m.reconcileSelection()
		return m, nil
	default:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.reconcileSelection()
		return m, cmd
	}
}

func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	delta := 0
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		delta = -3
	case tea.MouseButtonWheelDown:
		delta = 3
	default:
		return m, nil
	}
	if m.overlay == overlayHelp {
		m.scrollHelp(delta)
		return m, nil
	}
	if m.overlay != overlayNone {
		return m, nil
	}
	if m.view == viewDetail {
		m.scrollDetail(delta)
		return m, nil
	}
	switch m.view {
	case viewRules:
		m.moveSelection(delta)
	case viewPrecheck:
		itemCount := 0
		if m.precheckResult != nil {
			itemCount = len(m.precheckResult.Precheck.Items)
		}
		m.resultSelected = max(0, min(m.resultSelected+delta, max(itemCount-1, 0)))
	case viewApplyResult:
		itemCount := 0
		if m.applyResult != nil {
			itemCount = len(m.applyResult.Result.Items)
		}
		m.resultSelected = max(0, min(m.resultSelected+delta, max(itemCount-1, 0)))
	}
	return m, nil
}

func (m *Model) computeRates(newStats *StatsResponse, sampledAt time.Time) {
	if newStats == nil {
		return
	}
	elapsed := sampledAt.Sub(m.lastStatsAt).Seconds()

	currentRules := make(map[string]struct{}, len(newStats.Items))
	for _, snapshot := range newStats.Items {
		currentRules[snapshot.RuleID] = struct{}{}
	}
	for ruleID := range m.rates {
		if _, exists := currentRules[ruleID]; !exists {
			delete(m.rates, ruleID)
		}
	}
	for ruleID := range m.history {
		if _, exists := currentRules[ruleID]; !exists {
			delete(m.history, ruleID)
		}
	}

	previousByRule := make(map[string]TrafficSnapshot)
	if m.stats != nil {
		previousByRule = make(map[string]TrafficSnapshot, len(m.stats.Items))
		for _, snapshot := range m.stats.Items {
			previousByRule[snapshot.RuleID] = snapshot
		}
	}

	for _, current := range newStats.Items {
		rate := rateEntry{}
		previous, exists := previousByRule[current.RuleID]
		if exists && elapsed > 0 {
			if current.UploadBytes >= previous.UploadBytes {
				rate.UploadRate = float64(current.UploadBytes-previous.UploadBytes) / elapsed
			}
			if current.DownloadBytes >= previous.DownloadBytes {
				rate.DownloadRate = float64(current.DownloadBytes-previous.DownloadBytes) / elapsed
			}
		}
		m.rates[current.RuleID] = rate
		history := m.history[current.RuleID]
		if history == nil {
			history = &rateHistory{}
			m.history[current.RuleID] = history
		}
		history.push(rate.UploadRate, rate.DownloadRate)
	}
}

func (m Model) filteredRules() []RuleInfo {
	rules := m.displayRules()
	filter := strings.ToLower(strings.TrimSpace(m.filterInput.Value()))
	if filter == "" {
		return rules
	}
	result := make([]RuleInfo, 0, len(rules))
	for _, rule := range rules {
		if strings.Contains(strings.ToLower(rule.Name), filter) || strings.Contains(strings.ToLower(rule.RuleID), filter) {
			result = append(result, rule)
		}
	}
	return result
}

func (m Model) sortedRules() []RuleInfo {
	rules := m.filteredRules()
	stats := m.statsMap()
	sort.SliceStable(rules, func(i, j int) bool {
		comparison := 0
		left := stats[rules[i].RuleID]
		right := stats[rules[j].RuleID]
		switch m.sort {
		case sortConns:
			comparison = compareInt64(left.Conns, right.Conns)
		case sortUpload:
			comparison = compareInt64(left.UploadBytes, right.UploadBytes)
		case sortDownload:
			comparison = compareInt64(left.DownloadBytes, right.DownloadBytes)
		default:
			comparison = strings.Compare(strings.ToLower(rules[i].Name), strings.ToLower(rules[j].Name))
		}
		if comparison == 0 {
			comparison = strings.Compare(rules[i].RuleID, rules[j].RuleID)
		}
		if m.sortDesc {
			return comparison > 0
		}
		return comparison < 0
	})
	return rules
}

func compareInt64(left, right int64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func (m Model) statsMap() map[string]TrafficSnapshot {
	result := make(map[string]TrafficSnapshot)
	if m.stats != nil {
		for _, snapshot := range m.stats.Items {
			result[snapshot.RuleID] = snapshot
		}
	}
	return result
}

func (m Model) selectedRule() *RuleInfo {
	rules := m.sortedRules()
	if len(rules) == 0 {
		return nil
	}
	for index := range rules {
		if rules[index].RuleID == m.selectedRuleID {
			return &rules[index]
		}
	}
	return &rules[0]
}

func (m Model) selectedIndex() int {
	rules := m.sortedRules()
	for index := range rules {
		if rules[index].RuleID == m.selectedRuleID {
			return index
		}
	}
	return 0
}

func (m *Model) reconcileSelection() {
	rules := m.sortedRules()
	if len(rules) == 0 {
		if m.selectedRuleID != "" {
			m.contentYOffset = 0
		}
		m.selectedRuleID = ""
		return
	}
	for _, rule := range rules {
		if rule.RuleID == m.selectedRuleID {
			return
		}
	}
	if m.selectedRuleID != rules[0].RuleID {
		m.contentYOffset = 0
		m.selectedRuleID = rules[0].RuleID
	}
}

func (m *Model) moveSelection(delta int) {
	rules := m.sortedRules()
	if len(rules) == 0 {
		m.selectedRuleID = ""
		return
	}
	m.selectRuleAt(m.selectedIndex() + delta)
}

func (m *Model) selectRuleAt(index int) {
	rules := m.sortedRules()
	if len(rules) == 0 {
		m.selectedRuleID = ""
		return
	}
	index = max(0, min(index, len(rules)-1))
	if m.selectedRuleID != rules[index].RuleID {
		m.contentYOffset = 0
		m.selectedRuleID = rules[index].RuleID
	}
}

func (m *Model) cycleSort() {
	for index, key := range sortKeys {
		if key == m.sort {
			m.sort = sortKeys[(index+1)%len(sortKeys)]
			break
		}
	}
	m.sortDesc = !m.sortDesc
	m.reconcileSelection()
}

func (m *Model) setStatus(text string) {
	m.statusText = text
	m.statusTime = time.Now()
}

func (m *Model) beginStatsRequest() int64 {
	m.statsRequestID++
	return m.statsRequestID
}

func (m *Model) beginRulesRequest() int64 {
	m.rulesRequestID++
	return m.rulesRequestID
}

func (m *Model) beginSessionRequest() int64 {
	m.sessionRequestID++
	return m.sessionRequestID
}

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func fetchStatsCmd(ctx context.Context, client *Client, requestIDs ...int64) tea.Cmd {
	requestID := optionalRequestID(requestIDs)
	return func() tea.Msg {
		resp, err := client.Stats(ctx)
		return statsMsg{requestID: requestID, resp: resp, err: err}
	}
}

func fetchRulesCmd(ctx context.Context, client *Client, requestIDs ...int64) tea.Cmd {
	requestID := optionalRequestID(requestIDs)
	return func() tea.Msg {
		resp, err := client.Rules(ctx)
		return rulesMsg{requestID: requestID, resp: resp, err: err}
	}
}

func fetchSessionCmd(ctx context.Context, client *Client, requestIDs ...int64) tea.Cmd {
	requestID := optionalRequestID(requestIDs)
	return func() tea.Msg {
		resp, err := client.Session(ctx)
		return sessionMsg{requestID: requestID, resp: resp, err: err}
	}
}

func optionalRequestID(requestIDs []int64) int64 {
	if len(requestIDs) == 0 {
		return 0
	}
	return requestIDs[0]
}

func fetchConfigRulesCmd(ctx context.Context, client *Client, requestID int64) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.ConfigRules(ctx)
		return configRulesMsg{requestID: requestID, resp: resp, err: err}
	}
}

func fetchBotConfigCmd(ctx context.Context, client *Client, requestID int64) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.BotConfig(ctx)
		return botConfigMsg{
			requestID: requestID,
			operation: botOperationFetching,
			resp:      resp,
			err:       err,
		}
	}
}

func precheckCmd(ctx context.Context, client *Client, etag string, draft ConfigRulesRequest) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.Precheck(ctx, etag, draft)
		return precheckMsg{resp: resp, err: err}
	}
}

func applyCmd(ctx context.Context, client *Client, etag string, draft ConfigRulesRequest) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.Apply(ctx, etag, draft)
		return applyMsg{resp: resp, err: err}
	}
}

func reloadCmd(ctx context.Context, client *Client) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.Reload(ctx)
		return reloadMsg{resp: resp, err: err}
	}
}

// controlErrorMessage maps control API and network errors to a user-facing hint.
func controlErrorMessage(err error) string {
	switch apiStatus(err) {
	case http.StatusPreconditionFailed:
		return "config changed by another path; refresh and retry"
	case http.StatusForbidden:
		return "admin role required"
	case http.StatusUnprocessableEntity:
		return "precheck failed (port conflict or invalid config)"
	case http.StatusServiceUnavailable:
		return "operation failed; runtime rolled back"
	case 0:
		return friendlyError(err)
	default:
		return err.Error()
	}
}
