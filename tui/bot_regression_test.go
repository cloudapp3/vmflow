package tui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestBotInitialFetchFailureRendersPersistentError(t *testing.T) {
	m := managedTestModel()
	m.width = 80
	m.height = 24
	m.view = viewBotConfig
	m.botConfig = nil
	m.botRequestID = 1
	m.botOperation = botOperationFetching

	updated, cmd := m.Update(botConfigMsg{
		requestID: 1,
		operation: botOperationFetching,
		err:       errors.New("dial tcp: connection refused"),
	})
	m = updated.(Model)
	if cmd != nil || m.botOperation != botOperationIdle {
		t.Fatalf("failed fetch state: operation=%v cmd=%v", m.botOperation, cmd)
	}

	output := ansi.Strip(m.View())
	if strings.Contains(output, "Loading bot configuration") {
		t.Fatalf("failed initial fetch remained in loading state:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "unavailable") {
		t.Fatalf("failed initial fetch did not render an unavailable state:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "retry") {
		t.Fatalf("failed initial fetch did not offer a retry action:\n%s", output)
	}
}

func TestBotSaveConflictRebasesLocalEditsOntoLatestConfig(t *testing.T) {
	paths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		switch r.URL.Path {
		case "/v1/config/bot":
			w.Header().Set("ETag", `"bot-rev-2"`)
			_, _ = w.Write([]byte(`{"revision":"bot-rev-2","bot_token":"123:remote","bot_chat":84,"bot_control_token":"control-remote","running":true}`))
		case "/v1/config/rules":
			w.Header().Set("ETag", `"bot-rev-2"`)
			_, _ = w.Write([]byte(`{"revision":"bot-rev-2","writable":true,"udp_max_sessions":256,"rules":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	m := managedTestModel()
	m.client = NewClient(server.URL)
	m.view = viewBotEditor
	m.botConfig = &BotConfigResponse{
		Revision:        "bot-rev-1",
		ETag:            `"bot-rev-1"`,
		BotToken:        "123:old",
		BotChat:         42,
		BotControlToken: "control-old",
	}
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.botEditor.token.SetValue("123:local")
	m.botRequestID = 7
	m.botOperation = botOperationSaving

	updated, refresh := m.Update(botConfigMsg{
		requestID: 7,
		operation: botOperationSaving,
		err: &APIError{
			StatusCode: http.StatusPreconditionFailed,
			Message:    "configuration revision changed",
		},
	})
	m = updated.(Model)
	if refresh == nil || m.botOperation != botOperationFetching {
		t.Fatalf("412 did not start a refresh: operation=%v cmd=%v status=%q", m.botOperation, refresh, m.statusText)
	}
	if m.view != viewBotEditor || m.botEditor == nil || m.botEditor.token.Value() != "123:local" {
		t.Fatalf("412 discarded local edits: view=%v editor=%+v", m.view, m.botEditor)
	}

	rawBatch := refresh()
	batch, ok := rawBatch.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("412 reconciliation batch=%T len=%d, want bot and shared-config fetches", rawBatch, len(batch))
	}
	botMessage, ok := batch[0]().(botConfigMsg)
	if !ok || botMessage.requestID != m.botRequestID || botMessage.operation != botOperationFetching || botMessage.err != nil {
		t.Fatalf("bot reconciliation message=%+v", botMessage)
	}
	configMessage, ok := batch[1]().(configRulesMsg)
	if !ok || configMessage.requestID != m.configBarrierID || configMessage.err != nil {
		t.Fatalf("shared config reconciliation message=%+v barrier=%d", configMessage, m.configBarrierID)
	}
	if first, second := <-paths, <-paths; first != "/v1/config/bot" || second != "/v1/config/rules" {
		t.Fatalf("reconciliation paths=%q, %q", first, second)
	}

	updated, cmd := m.Update(botMessage)
	m = updated.(Model)
	if cmd != nil || m.botOperation != botOperationIdle {
		t.Fatalf("rebase refresh did not settle: operation=%v cmd=%v", m.botOperation, cmd)
	}
	if m.view != viewBotEditor || m.botEditor == nil {
		t.Fatalf("rebase closed the active editor: view=%v editor=%+v", m.view, m.botEditor)
	}
	if got := m.botEditor.token.Value(); got != "123:local" {
		t.Fatalf("locally edited token = %q, want %q", got, "123:local")
	}
	if got := m.botEditor.chat.Value(); got != "84" {
		t.Fatalf("untouched chat ID = %q, want latest remote value", got)
	}
	if got := m.botEditor.control.Value(); got != "control-remote" {
		t.Fatalf("untouched control token = %q, want latest remote value", got)
	}
	if got := m.botEditor.etag; got != `"bot-rev-2"` {
		t.Fatalf("editor ETag = %q, want latest ETag", got)
	}
	if m.botConfig == nil || m.botConfig.Revision != "bot-rev-2" {
		t.Fatalf("latest bot config was not retained: %+v", m.botConfig)
	}
}

func TestBotEditorEscapeProtectsDirtyEdits(t *testing.T) {
	base := &BotConfigResponse{
		Revision:        "bot-rev-1",
		ETag:            `"bot-rev-1"`,
		BotToken:        "123:old",
		BotChat:         42,
		BotControlToken: "control-old",
	}

	t.Run("dirty editor requires confirmation", func(t *testing.T) {
		m := managedTestModel()
		m.view = viewBotEditor
		m.botConfig = base
		m.botEditor = newBotConfigEditor(base)
		m.botEditor.token.SetValue("123:local")

		updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		m = updated.(Model)
		if cmd != nil || m.overlay == overlayNone {
			t.Fatalf("dirty escape did not open confirmation: overlay=%v cmd=%v", m.overlay, cmd)
		}
		if m.view != viewBotEditor || m.botEditor == nil || m.botEditor.token.Value() != "123:local" {
			t.Fatalf("dirty escape discarded editor before confirmation: view=%v editor=%+v", m.view, m.botEditor)
		}
		updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
		m = updated.(Model)
		if cmd != nil || m.overlay != overlayNone || m.botEditor == nil {
			t.Fatalf("canceling discard confirmation lost editor: overlay=%v editor=%+v cmd=%v", m.overlay, m.botEditor, cmd)
		}
		updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = updated.(Model)
		if cmd != nil || m.overlay != overlayCancelBotEditor || m.botEditor == nil {
			t.Fatalf("dirty Ctrl+C did not reopen confirmation: overlay=%v editor=%+v cmd=%v", m.overlay, m.botEditor, cmd)
		}

		updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		m = updated.(Model)
		if cmd != nil || m.overlay != overlayNone || m.view != viewBotConfig || m.botEditor != nil {
			t.Fatalf("confirmed cancellation did not close editor: overlay=%v view=%v editor=%+v cmd=%v", m.overlay, m.view, m.botEditor, cmd)
		}
	})

	t.Run("clean editor closes immediately", func(t *testing.T) {
		m := managedTestModel()
		m.view = viewBotEditor
		m.botConfig = base
		m.botEditor = newBotConfigEditor(base)

		updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		m = updated.(Model)
		if cmd != nil || m.overlay != overlayNone || m.view != viewBotConfig || m.botEditor != nil {
			t.Fatalf("clean escape did not close immediately: overlay=%v view=%v editor=%+v cmd=%v", m.overlay, m.view, m.botEditor, cmd)
		}
	})
}

func TestBotCachedConfigFetchFailureRendersStaleAndRetry(t *testing.T) {
	m := managedTestModel()
	m.width = 80
	m.height = 24
	m.view = viewBotConfig
	m.botConfig = &BotConfigResponse{
		Revision: "bot-rev-1",
		ETag:     `"bot-rev-1"`,
		BotToken: "123:cached",
		BotChat:  42,
		Running:  true,
	}
	m.botRequestID = 3
	m.botOperation = botOperationFetching

	updated, cmd := m.Update(botConfigMsg{
		requestID: 3,
		operation: botOperationFetching,
		err:       errors.New("context deadline exceeded"),
	})
	m = updated.(Model)
	if cmd != nil || m.botOperation != botOperationIdle || m.botConfig == nil {
		t.Fatalf("cached fetch failure state: operation=%v config=%+v cmd=%v", m.botOperation, m.botConfig, cmd)
	}
	if m.botConfigErr == nil {
		t.Fatal("cached fetch failure was not retained")
	}

	output := ansi.Strip(m.View())
	if !strings.Contains(output, "STALE") || !strings.Contains(output, "Chat ID") || !strings.Contains(output, "42") {
		t.Fatalf("cached config was not rendered as stale:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "retry") {
		t.Fatalf("stale bot config did not offer retry:\n%s", output)
	}
}

func TestBotRebaseFetchFailureBlocksSaveAndAllowsRetry(t *testing.T) {
	m := managedTestModel()
	m.view = viewBotEditor
	m.botConfig = &BotConfigResponse{
		Revision:        "bot-rev-1",
		ETag:            `"bot-rev-1"`,
		BotToken:        "123:old",
		BotChat:         42,
		BotControlToken: "control-old",
	}
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.botEditor.token.SetValue("123:local")
	m.botRequestID = 11
	m.botOperation = botOperationSaving

	updated, refresh := m.Update(botConfigMsg{
		requestID: 11,
		operation: botOperationSaving,
		err: &APIError{
			StatusCode: http.StatusPreconditionFailed,
			Message:    "configuration revision changed",
		},
	})
	m = updated.(Model)
	if refresh == nil || m.botOperation != botOperationFetching || !m.botRebasePending {
		t.Fatalf("412 did not start rebase fetch: operation=%v pending=%v cmd=%v", m.botOperation, m.botRebasePending, refresh)
	}

	rebaseRequestID := m.botRequestID
	updated, cmd := m.Update(botConfigMsg{
		requestID: rebaseRequestID,
		operation: botOperationFetching,
		err:       errors.New("dial tcp: connection refused"),
	})
	m = updated.(Model)
	if cmd != nil || m.botOperation != botOperationIdle || !m.botRebasePending || m.botConfigErr == nil {
		t.Fatalf("failed rebase fetch state: operation=%v pending=%v err=%v cmd=%v", m.botOperation, m.botRebasePending, m.botConfigErr, cmd)
	}
	if m.view != viewBotEditor || m.botEditor == nil || m.botEditor.token.Value() != "123:local" {
		t.Fatalf("failed rebase fetch discarded editor: view=%v editor=%+v", m.view, m.botEditor)
	}
	if !strings.Contains(strings.ToLower(m.botEditor.formError), "ctrl+r") {
		t.Fatalf("failed rebase fetch form error omitted retry hint: %q", m.botEditor.formError)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = updated.(Model)
	if m.botEditor == nil || !strings.Contains(strings.ToLower(m.botEditor.formError), "ctrl+r") {
		t.Fatalf("editing after failed rebase hid retry guidance: editor=%+v", m.botEditor)
	}
	localToken := m.botEditor.token.Value()

	requestIDBeforeSave := m.botRequestID
	updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if cmd != nil || m.botOperation != botOperationIdle || m.botRequestID != requestIDBeforeSave {
		t.Fatalf("save was not blocked before rebase: operation=%v request=%d cmd=%v", m.botOperation, m.botRequestID, cmd)
	}
	if m.botEditor == nil || !strings.Contains(strings.ToLower(m.botEditor.formError), "ctrl+r") {
		t.Fatalf("blocked save lost retry guidance: editor=%+v", m.botEditor)
	}

	updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(Model)
	if cmd == nil || m.botOperation != botOperationFetching || !m.botRebasePending || m.botRequestID != requestIDBeforeSave+1 {
		t.Fatalf("Ctrl+R did not retry rebase: operation=%v pending=%v request=%d cmd=%v", m.botOperation, m.botRebasePending, m.botRequestID, cmd)
	}
	if m.botEditor == nil || m.botEditor.token.Value() != localToken {
		t.Fatalf("Ctrl+R retry discarded local edit: editor=%+v", m.botEditor)
	}
}

func TestBotSaveErrorsPersistUntilAFieldIsEdited(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "unprocessable entity",
			err: &APIError{
				StatusCode: http.StatusUnprocessableEntity,
				Message:    "invalid bot configuration",
			},
		},
		{
			name: "readiness",
			err: &APIError{
				StatusCode: http.StatusServiceUnavailable,
				Message:    "bot readiness could not be established",
				Body:       []byte(`{"error":"bot readiness could not be established","running":true}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := managedTestModel()
			m.width = 80
			m.height = 24
			m.view = viewBotEditor
			m.botConfig = &BotConfigResponse{
				Revision: "bot-rev-1",
				ETag:     `"bot-rev-1"`,
				BotToken: "123:old",
				BotChat:  42,
			}
			m.botEditor = newBotConfigEditor(m.botConfig)
			m.botRequestID = 5
			m.botOperation = botOperationSaving

			updated, cmd := m.Update(botConfigMsg{
				requestID: 5,
				operation: botOperationSaving,
				err:       tt.err,
			})
			m = updated.(Model)
			if cmd != nil || m.botOperation != botOperationIdle || m.botEditor == nil || m.botEditor.formError == "" {
				t.Fatalf("save error was not retained in editor: operation=%v editor=%+v cmd=%v", m.botOperation, m.botEditor, cmd)
			}
			formError := m.botEditor.formError
			if !strings.Contains(ansi.Strip(m.View()), formError) {
				t.Fatalf("form error was not rendered: %q\n%s", formError, ansi.Strip(m.View()))
			}

			updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
			m = updated.(Model)
			if m.botEditor == nil || m.botEditor.formError != formError {
				t.Fatalf("cursor movement cleared form error: before=%q after=%q", formError, m.botEditor.formError)
			}

			before := m.botEditor.token.Value()
			updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
			m = updated.(Model)
			if m.botEditor == nil || m.botEditor.token.Value() == before || m.botEditor.formError != "" {
				t.Fatalf("field edit did not clear form error: before=%q editor=%+v", before, m.botEditor)
			}
		})
	}
}

func TestBotActionFailureStartsStateReconciliation(t *testing.T) {
	tests := []struct {
		operation botOperationState
		action    string
	}{
		{operation: botOperationStarting, action: "start"},
		{operation: botOperationStopping, action: "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			m := managedTestModel()
			m.view = viewBotConfig
			m.botConfig = &BotConfigResponse{BotToken: "123:token", BotChat: 42, Running: tt.action == "stop"}
			m.botRequestID = 9
			m.botOperation = tt.operation

			updated, cmd := m.Update(botActionMsg{
				requestID: 9,
				operation: tt.operation,
				action:    tt.action,
				err:       errors.New("connection reset"),
			})
			m = updated.(Model)
			if cmd == nil || m.botOperation != botOperationFetching || m.botRequestID != 10 {
				t.Fatalf("%s failure did not start reconciliation: operation=%v request=%d cmd=%v", tt.action, m.botOperation, m.botRequestID, cmd)
			}
			if !strings.Contains(m.statusText, "failed") || !strings.Contains(m.statusText, "refreshing state") {
				t.Fatalf("%s failure status=%q", tt.action, m.statusText)
			}

			updated, _ = m.Update(botConfigMsg{
				requestID: m.botRequestID,
				operation: botOperationFetching,
				resp: &BotConfigResponse{
					BotToken: "123:token",
					BotChat:  42,
					Running:  tt.action == "start",
				},
			})
			m = updated.(Model)
			wantState := "STOPPED"
			if tt.action == "start" {
				wantState = "RUNNING"
			}
			if m.botReconcileAction != "" || !strings.Contains(m.statusText, "reconciled: "+wantState) || strings.Contains(m.statusText, "failed") {
				t.Fatalf("%s reconciliation status=%q pending=%q", tt.action, m.statusText, m.botReconcileAction)
			}
		})
	}
}

func TestBotPanelTickRefreshesCurrentState(t *testing.T) {
	pathCh := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathCh <- r.URL.Path
		w.Header().Set("ETag", `"bot-rev-2"`)
		_, _ = w.Write([]byte(`{"revision":"bot-rev-2","bot_token":"123:token","bot_chat":42,"running":true}`))
	}))
	defer server.Close()

	m := newModel(t.Context(), server.URL)
	m.ready = true
	m.width = 80
	m.height = 24
	m.view = viewBotConfig
	m.botConfig = &BotConfigResponse{Revision: "bot-rev-1", BotToken: "123:token", BotChat: 42}
	previousRequestID := m.botRequestID

	updated, cmd := m.Update(tickMsg(time.Now()))
	m = updated.(Model)
	if cmd == nil || m.botOperation != botOperationFetching || m.botRequestID != previousRequestID+1 {
		t.Fatalf("bot panel tick did not start refresh: operation=%v request=%d cmd=%v", m.botOperation, m.botRequestID, cmd)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 5 {
		t.Fatalf("bot panel tick batch=%T len=%d, want five commands", cmd(), len(batch))
	}
	message, ok := batch[3]().(botConfigMsg)
	if !ok || message.operation != botOperationFetching || message.requestID != m.botRequestID || message.err != nil {
		t.Fatalf("bot refresh message=%+v", message)
	}
	if path := <-pathCh; path != "/v1/config/bot" {
		t.Fatalf("bot refresh path=%q", path)
	}
}

func TestBotAction503MessagesMatchLifecycleOperation(t *testing.T) {
	start := botControlErrorMessage("start", &APIError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "bot readiness could not be established",
	})
	if !strings.Contains(start, "readiness") || !strings.Contains(start, "previous bot") {
		t.Fatalf("start readiness message=%q", start)
	}
	stop := botControlErrorMessage("stop", &APIError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "bot could not be stopped",
	})
	if strings.Contains(stop, "readiness") || !strings.Contains(stop, "state uncertain") {
		t.Fatalf("stop lifecycle message=%q", stop)
	}
}

func TestCompactBotViewsKeepRecoveryAndLifecycleActionsVisible(t *testing.T) {
	m := managedTestModel()
	m.width = 40
	m.height = 12
	m.view = viewBotConfig
	m.botConfig = &BotConfigResponse{BotToken: "123:token", BotChat: 42, Running: true}
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "RUNNING") || !strings.Contains(output, "[x]stop") || !strings.Contains(output, "[e]edit") {
		t.Fatalf("compact bot panel omitted current actions:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)

	m.view = viewBotEditor
	m.botEditor = newBotConfigEditor(m.botConfig)
	m.botEditor.token.SetValue("123:local")
	m.botEditor.formError = "Latest remote configuration is required; press Ctrl+R to retry."
	m.botRebasePending = true
	output = ansi.Strip(m.View())
	if !strings.Contains(output, "Latest remote") || !strings.Contains(output, "[ctrl+r]rebase") {
		t.Fatalf("compact bot editor omitted recovery guidance:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)

	m.botEditor.formError = ""
	m.botEditor.chatError = "must be an integer"
	output = ansi.Strip(m.View())
	if !strings.Contains(output, "must be an") {
		t.Fatalf("compact bot editor omitted Chat ID error:\n%s", output)
	}
	assertRenderBounds(t, m.View(), m.width, m.height)
}
