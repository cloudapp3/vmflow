package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/i18n"
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
	overlayLanguage
	overlayReload
	overlayDelete
	overlayApply
	overlayDiscardDraft
	overlayQuitDirty
	overlayCancelEditor
	overlayUDPSettings
	overlayBotStop
	overlayCancelBotEditor
	overlayStartService
	overlayElevateService
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
	tr     *i18n.Translator

	// Runtime monitoring data.
	stats        *StatsResponse
	prevStats    *StatsResponse
	rules        *RulesResponse
	rates        map[string]rateEntry
	networkRates map[string]rateEntry
	history      map[string]*rateHistory
	lastStatsAt  time.Time

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
	view            viewMode
	selectedRuleID  string
	sort            sortKey
	sortDesc        bool
	paused          bool
	filterActive    bool
	filterInput     textinput.Model
	udpInput        textinput.Model
	udpInputError   string
	statusText      string
	statusTime      time.Time
	contentYOffset  int
	overlayYOffset  int
	networkExpanded bool

	// Request generations prevent slower responses from replacing newer state.
	statsRequestID       int64
	rulesRequestID       int64
	sessionRequestID     int64
	configRequestID      int64
	lastConfigResponseID int64
	configBarrierID      int64
	botRequestID         int64
	serviceRequestID     int64

	// Native service recovery is enabled only for a known-local target.
	serviceController        *ServiceController
	serviceState             serviceRecoveryState
	serviceSummary           ServiceSummary
	serviceErr               error
	serviceReconnectDeadline time.Time

	width  int
	height int

	refreshInterval time.Duration
	spinner         spinner.Model
	viewport        viewport.Model
	ready           bool
}

// RunOptions controls presentation and optional local-service recovery for a
// TUI session.
type RunOptions struct {
	Translator        *i18n.Translator
	ServiceController *ServiceController
}

func Run(ctx context.Context, stdout io.Writer, addr, token string, httpClient *http.Client, headers http.Header) error {
	return RunWithOptions(ctx, stdout, addr, token, httpClient, headers, RunOptions{})
}

func RunWithOptions(ctx context.Context, stdout io.Writer, addr, token string, httpClient *http.Client, headers http.Header, options RunOptions) error {
	m := newModelWithOptions(ctx, addr, options, token)
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
	return newModelWithOptions(ctx, addr, RunOptions{}, token...)
}

func newModelWithOptions(ctx context.Context, addr string, options RunOptions, token ...string) Model {
	translator := options.Translator
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = subtleStyle

	filter := textinput.New()
	filter.Placeholder = translator.T("tui.input.filter.placeholder")
	filter.CharLimit = 80

	udpInput := textinput.New()
	udpInput.Prompt = ""
	udpInput.Placeholder = translator.T("tui.input.udp_sessions.placeholder")
	udpInput.CharLimit = 5
	udpInput.Width = 16

	serviceState := serviceRecoveryDisabled
	if options.ServiceController != nil {
		serviceState = serviceRecoveryIdle
	}

	return Model{
		ctx:               ctx,
		client:            NewClient(addr, token...),
		tr:                translator,
		rates:             make(map[string]rateEntry),
		networkRates:      make(map[string]rateEntry),
		history:           make(map[string]*rateHistory),
		view:              viewDashboard,
		sort:              sortName,
		sync:              syncUnavailable,
		refreshInterval:   3 * time.Second,
		spinner:           s,
		filterInput:       filter,
		udpInput:          udpInput,
		serviceController: options.ServiceController,
		serviceState:      serviceState,
		statsRequestID:    1,
		rulesRequestID:    1,
		sessionRequestID:  1,
		configRequestID:   1,
	}
}

func (m Model) t(key string) string {
	if m.tr == nil {
		return i18n.New(i18n.English).T(key)
	}
	return m.tr.T(key)
}

func (m Model) tf(key string, args ...any) string {
	if m.tr == nil {
		return i18n.New(i18n.English).Tf(key, args...)
	}
	return m.tr.Tf(key, args...)
}

func (m *Model) openLanguageSelector() {
	m.overlayYOffset = 0
	m.overlay = overlayLanguage
}

func (m *Model) setLanguage(locale string) {
	matched, ok := i18n.Match(locale)
	if !ok {
		return
	}
	translator := i18n.New(matched)
	m.tr = translator
	m.filterInput.Placeholder = translator.T("tui.input.filter.placeholder")
	m.udpInput.Placeholder = translator.T("tui.input.udp_sessions.placeholder")
	if m.udpInputError != "" {
		m.udpInputError = translator.Tf("tui.validation.udp_sessions", engine.MaxUDPGlobalMaxSessions)
	}
	if m.editor != nil {
		m.editor.setTranslator(translator)
	}
	if m.botEditor != nil {
		m.botEditor.setTranslator(translator)
	}
	m.overlay = overlayNone
	m.overlayYOffset = 0
	languageKey := "tui.language.english"
	if matched == i18n.SimplifiedChinese {
		languageKey = "tui.language.simplified_chinese"
	}
	m.setStatus(m.tf("tui.status.language_changed", m.t(languageKey)))
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
			if m.shouldInspectService(msg.err) {
				return m, m.beginServiceInspection()
			}
		} else {
			wasRecovering := m.serviceState != serviceRecoveryDisabled && m.serviceState != serviceRecoveryIdle
			m.connectionErr = nil
			sampledAt := time.Now()
			m.computeRates(msg.resp, sampledAt)
			m.prevStats = m.stats
			m.stats = msg.resp
			m.lastStatsAt = sampledAt
			if wasRecovering {
				m.resetServiceRecovery()
				m.setStatus(m.t("tui.status.service_connected"))
			}
		}
		return m, nil

	case serviceInspectMsg:
		if msg.requestID != m.serviceRequestID || m.serviceState != serviceRecoveryChecking {
			return m, nil
		}
		m.serviceSummary = msg.summary
		m.serviceErr = msg.err
		switch {
		case msg.err != nil:
			m.serviceState = serviceRecoveryFailed
		case !msg.summary.Installed:
			m.serviceState = serviceRecoveryNotInstalled
		case msg.summary.Running:
			m.serviceState = serviceRecoveryRunning
		default:
			m.serviceState = serviceRecoveryStopped
		}
		return m, nil

	case serviceStartMsg:
		if msg.requestID != m.serviceRequestID || m.serviceState != serviceRecoveryStarting {
			return m, nil
		}
		if msg.err != nil {
			m.serviceErr = msg.err
			switch {
			case !msg.elevated && errors.Is(msg.err, ErrServicePrivilegesRequired) && m.serviceController != nil && m.serviceController.ElevatedStartCommand != nil:
				m.serviceState = serviceRecoveryStopped
				m.overlay = overlayElevateService
				m.setStatus(m.t("tui.status.service_privileges_required"))
			case errors.Is(msg.err, ErrServiceNotInstalled):
				m.serviceSummary = ServiceSummary{State: "not installed"}
				m.serviceState = serviceRecoveryNotInstalled
				m.setStatus(m.t("tui.status.service_not_installed"))
			default:
				m.serviceState = serviceRecoveryFailed
				m.setStatus(m.tf("tui.status.service_start_failed", msg.err))
			}
			return m, nil
		}
		m.serviceState = serviceRecoveryWaiting
		m.serviceSummary.Running = true
		m.serviceErr = nil
		m.serviceReconnectDeadline = time.Now().Add(serviceReconnectTimeout)
		m.setStatus(m.t("tui.status.service_waiting"))
		commands := m.refreshCommands()
		commands = append(commands, serviceReconnectCmd(m.serviceRequestID))
		return m, tea.Batch(commands...)

	case serviceReconnectMsg:
		if msg.requestID != m.serviceRequestID || m.serviceState != serviceRecoveryWaiting {
			return m, nil
		}
		if !m.serviceReconnectDeadline.IsZero() && time.Now().After(m.serviceReconnectDeadline) {
			m.serviceState = serviceRecoveryRunning
			m.setStatus(m.t("tui.status.service_management_unreachable"))
			return m, nil
		}
		commands := m.refreshCommands()
		commands = append(commands, serviceReconnectCmd(msg.requestID))
		return m, tea.Batch(commands...)

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
				message := m.tf("tui.status.bot_config_unavailable", m.friendlyError(msg.err))
				if m.botReconcileAction != "" {
					message = m.tf("tui.status.bot_reconcile_failed", m.botActionLabel(m.botReconcileAction), m.friendlyError(msg.err))
				}
				if m.botRebasePending && m.botEditor != nil {
					message = m.tf("tui.status.bot_latest_load_failed", m.friendlyError(msg.err))
					m.botEditor.formError = m.tf("tui.status.retry_ctrl_r", message)
				}
				m.setStatus(message)
				return m, nil
			}
			switch apiStatus(msg.err) {
			case http.StatusPreconditionFailed:
				m.botRebasePending = true
				if m.botEditor != nil {
					m.botEditor.formError = m.t("tui.status.bot_remote_changed_loading")
				}
				m.setStatus(m.t("tui.status.bot_remote_changed"))
				botRequestID := m.beginBotOperation(botOperationFetching)
				configRequestID := m.beginConfigBarrier()
				return m, tea.Batch(
					fetchBotConfigCmd(m.ctx, m.client, botRequestID),
					fetchConfigRulesCmd(m.ctx, m.client, configRequestID),
				)
			case http.StatusForbidden:
				message := m.t("tui.status.bot_admin_required")
				if m.botEditor != nil {
					m.botEditor.formError = message
				}
				m.setStatus(message)
			case http.StatusUnprocessableEntity:
				message := m.t("tui.status.bot_config_rejected")
				if m.botEditor != nil {
					m.botEditor.formError = message
				}
				m.setStatus(message)
			case http.StatusServiceUnavailable:
				message, refresh, committed := m.botConfigUnavailableState(msg.err)
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
				message := m.tf("tui.status.bot_result_uncertain", m.friendlyError(msg.err))
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
			m.botEditor.formError = m.t("tui.status.bot_latest_loaded_editor")
			m.botRebasePending = false
			m.setStatus(m.t("tui.status.bot_latest_loaded"))
			return m, nil
		}
		if msg.operation == botOperationFetching {
			m.botRebasePending = false
			if m.botReconcileAction != "" {
				state := m.t("tui.state.stopped")
				if msg.resp != nil && msg.resp.Running {
					state = m.t("tui.state.running")
				}
				m.setStatus(m.tf("tui.status.bot_reconciled", m.botActionLabel(m.botReconcileAction), state))
				m.botReconcileAction = ""
			}
		}
		if msg.operation == botOperationSaving && m.view == viewBotEditor {
			m.botEditor = nil
			m.view = viewBotConfig
			m.botRebasePending = false
			m.setStatus(m.t("tui.status.bot_config_saved"))
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
			m.setStatus(m.tf("tui.status.bot_action_failed", m.botActionLabel(msg.action), m.botControlErrorMessage(msg.action, msg.err)))
			requestID := m.beginBotOperation(botOperationFetching)
			return m, fetchBotConfigCmd(m.ctx, m.client, requestID)
		}
		m.botReconcileAction = ""
		m.setStatus(m.tf("tui.status.bot_action_ok", m.botActionLabel(msg.action)))
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
			m.setStatus(m.tf("tui.status.reload_failed", m.controlErrorMessage(msg.err)))
			return m, nil
		}
		m.setStatus(m.tf("tui.status.reload_ok", msg.resp.RuleCount))
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

func (m Model) botControlErrorMessage(action string, err error) string {
	switch apiStatus(err) {
	case http.StatusForbidden:
		return m.t("tui.error.admin_required")
	case http.StatusUnprocessableEntity:
		return m.t("tui.error.bot_settings_invalid")
	case http.StatusServiceUnavailable:
		message := strings.ToLower(err.Error())
		if action == "start" && strings.Contains(message, "readiness") {
			return m.t("tui.error.bot_readiness")
		}
		if strings.Contains(message, "controller unavailable") {
			return m.t("tui.error.bot_controller_unavailable")
		}
		return m.t("tui.error.bot_lifecycle_uncertain")
	case 0:
		return m.friendlyError(err)
	default:
		return err.Error()
	}
}

func (m Model) botActionLabel(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		return m.t("tui.action.start")
	case "stop":
		return m.t("tui.action.stop")
	default:
		return action
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.overlay == overlayLanguage {
		return m.handleOverlayKey(msg)
	}
	if m.serviceBusy() {
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		default:
			m.setStatus(m.t("tui.status.service_operation_in_progress"))
			return m, nil
		}
	}
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
		if updated, cmd, handled := m.handleNavigationKey(msg); handled {
			return updated, cmd
		}
		return m.handleBotConfigKey(msg)
	}
	if m.filterActive {
		return m.handleFilterInput(msg)
	}
	if updated, cmd, handled := m.handleNavigationKey(msg); handled {
		return updated, cmd
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
	case "l":
		m.openLanguageSelector()
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
			m.setStatus(m.t("tui.status.monitoring_paused"))
		} else {
			m.setStatus(m.t("tui.status.monitoring_resumed"))
		}
	case "r":
		return m.refreshNow()
	case "+", "=":
		if m.refreshInterval > time.Second {
			m.refreshInterval -= time.Second
			m.setStatus(m.tf("tui.status.interval", int(m.refreshInterval.Seconds())))
		}
	case "-":
		if m.refreshInterval < 10*time.Second {
			m.refreshInterval += time.Second
			m.setStatus(m.tf("tui.status.interval", int(m.refreshInterval.Seconds())))
		}
	case "R":
		if m.draft != nil {
			m.setStatus(m.t("tui.status.discard_before_reload"))
		} else if !m.canReload() {
			m.setStatus(m.t("tui.status.reload_admin_required"))
		} else {
			m.overlay = overlayReload
		}
	case "b":
		return m.openBotConfig()
	case "a":
		if m.view == viewDashboard {
			m.networkExpanded = !m.networkExpanded
			m.contentYOffset = m.dashboardNetworkYOffset()
			if m.networkExpanded {
				m.setStatus(m.t("tui.status.network_showing_all"))
			} else {
				m.setStatus(m.t("tui.status.network_hiding_inactive"))
			}
		}
	case "up", "k":
		if m.view == viewDashboard || m.view == viewDetail {
			m.scrollContent(-1)
		} else if m.view == viewRules {
			m.moveSelection(-1)
		}
	case "down", "j":
		if m.view == viewDashboard || m.view == viewDetail {
			m.scrollContent(1)
		} else if m.view == viewRules {
			m.moveSelection(1)
		}
	case "pgup":
		if m.view == viewDashboard || m.view == viewDetail {
			m.scrollContent(-m.mainViewportHeight())
		} else if m.view == viewRules {
			m.moveSelection(-m.rulePageSize())
		}
	case "pgdown":
		if m.view == viewDashboard || m.view == viewDetail {
			m.scrollContent(m.mainViewportHeight())
		} else if m.view == viewRules {
			m.moveSelection(m.rulePageSize())
		}
	case "home":
		if m.view == viewDashboard || m.view == viewDetail {
			m.contentYOffset = 0
		} else if m.view == viewRules {
			m.selectRuleAt(0)
		}
	case "end":
		if m.view == viewDashboard || m.view == viewDetail {
			m.contentYOffset = m.contentMaxYOffset()
		} else if m.view == viewRules {
			m.selectRuleAt(len(m.sortedRules()) - 1)
		}
	case "enter":
		if m.view == viewDashboard && m.connectionErr != nil && m.canStartService() {
			m.overlay = overlayStartService
			return m, nil
		}
		if m.view == viewRules {
			switch {
			case m.draft != nil && m.sync == syncValidated && m.precheckResult != nil && m.precheckResult.Precheck.OK:
				m.overlay = overlayApply
			case m.draft != nil && m.sync == syncStale:
				m.setStatus(m.t("tui.status.stale_draft_before_precheck"))
			case m.draft != nil:
				return m.startPrecheck()
			case m.selectedRule() != nil:
				m.view = viewDetail
				m.contentYOffset = 0
			}
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

func (m Model) handleNavigationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch msg.String() {
	case "1":
		m.view = viewDashboard
		m.contentYOffset = 0
		return m, nil, true
	case "2":
		m.view = viewRules
		m.contentYOffset = 0
		return m, nil, true
	case "3":
		updated, cmd := m.openBotConfig()
		return updated, cmd, true
	case "4":
		m.view = viewDashboard
		m.contentYOffset = m.dashboardNetworkYOffset()
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m *Model) refreshCommands() []tea.Cmd {
	statsRequestID := m.beginStatsRequest()
	rulesRequestID := m.beginRulesRequest()
	sessionRequestID := m.beginSessionRequest()
	m.configRequestID++
	return []tea.Cmd{
		fetchStatsCmd(m.ctx, m.client, statsRequestID),
		fetchRulesCmd(m.ctx, m.client, rulesRequestID),
		fetchSessionCmd(m.ctx, m.client, sessionRequestID),
		fetchConfigRulesCmd(m.ctx, m.client, m.configRequestID),
	}
}

func (m Model) refreshNow() (tea.Model, tea.Cmd) {
	commands := m.refreshCommands()
	if m.serviceController != nil && m.connectionErr != nil && apiStatus(m.connectionErr) == 0 && !m.serviceBusy() {
		commands = append(commands, m.beginServiceInspection())
	}
	return m, tea.Batch(commands...)
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
	if m.view == viewDashboard || m.view == viewDetail {
		m.scrollContent(delta)
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
	m.computeNetworkRates(newStats.HostNetwork, elapsed)
}

func (m *Model) computeNetworkRates(current *HostNetworkSnapshot, elapsed float64) {
	currentInterfaces := make(map[string]struct{})
	if current != nil {
		currentInterfaces = make(map[string]struct{}, len(current.Interfaces))
		for _, iface := range current.Interfaces {
			currentInterfaces[iface.Name] = struct{}{}
		}
	}
	for name := range m.networkRates {
		if _, ok := currentInterfaces[name]; !ok {
			delete(m.networkRates, name)
		}
	}
	if current == nil {
		return
	}

	previousInterfaces := make(map[string]HostNetworkInterface)
	if m.stats != nil && m.stats.HostNetwork != nil {
		previousInterfaces = make(map[string]HostNetworkInterface, len(m.stats.HostNetwork.Interfaces))
		for _, iface := range m.stats.HostNetwork.Interfaces {
			previousInterfaces[iface.Name] = iface
		}
	}
	for _, iface := range current.Interfaces {
		rate := rateEntry{}
		previous, ok := previousInterfaces[iface.Name]
		if ok && elapsed > 0 && iface.CountersAvailable && previous.CountersAvailable {
			if iface.RXBytes >= previous.RXBytes {
				rate.DownloadRate = float64(iface.RXBytes-previous.RXBytes) / elapsed
			}
			if iface.TXBytes >= previous.TXBytes {
				rate.UploadRate = float64(iface.TXBytes-previous.TXBytes) / elapsed
			}
		}
		m.networkRates[iface.Name] = rate
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
		searchable := strings.Join([]string{
			rule.Name,
			rule.RuleID,
			formatEndpoint(rule.ListenAddr, rule.ListenPort),
			formatEndpoint(rule.TargetAddr, rule.TargetPort),
			rule.Remark,
		}, "\n")
		if strings.Contains(strings.ToLower(searchable), filter) {
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
	m.sortDesc = m.sort != sortName
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

func (m Model) controlErrorMessage(err error) string {
	switch apiStatus(err) {
	case http.StatusPreconditionFailed:
		return m.t("tui.error.config_changed")
	case http.StatusForbidden:
		return m.t("tui.error.admin_required")
	case http.StatusUnprocessableEntity:
		return m.t("tui.error.precheck_failed")
	case http.StatusServiceUnavailable:
		return m.t("tui.error.runtime_rolled_back")
	case 0:
		return m.friendlyError(err)
	default:
		return err.Error()
	}
}
