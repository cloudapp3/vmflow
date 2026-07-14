package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// botConfigEditor edits the Telegram bot configuration. Token fields are masked
// via textinput.EchoPassword so secrets are not shown on screen.
type botConfigEditor struct {
	token   textinput.Model
	chat    textinput.Model
	control textinput.Model
	focus   int
	etag    string
	base    BotConfigResponse

	chatError string
	formError string
}

func newBotConfigEditor(cfg *BotConfigResponse) *botConfigEditor {
	token := textinput.New()
	token.Prompt = ""
	token.EchoMode = textinput.EchoPassword
	token.SetValue(cfg.BotToken)
	token.CharLimit = 256
	token.Width = 40
	token.Placeholder = "Telegram bot token"

	chat := textinput.New()
	chat.Prompt = ""
	chat.SetValue(strconv.FormatInt(cfg.BotChat, 10))
	chat.CharLimit = 20
	chat.Width = 40
	chat.Placeholder = "chat ID"

	control := textinput.New()
	control.Prompt = ""
	control.EchoMode = textinput.EchoPassword
	control.SetValue(cfg.BotControlToken)
	control.CharLimit = 256
	control.Width = 40
	control.Placeholder = "admin token (blank = read-only bot)"

	e := &botConfigEditor{token: token, chat: chat, control: control, etag: cfg.ETag, base: *cfg}
	e.token.Focus()
	return e
}

func (e *botConfigEditor) fields() []*textinput.Model {
	return []*textinput.Model{&e.token, &e.chat, &e.control}
}

func (e *botConfigEditor) inputView(index, available int) string {
	if e == nil {
		return ""
	}
	fields := e.fields()
	if index < 0 || index >= len(fields) {
		return ""
	}
	return responsiveTextInputView(*fields[index], available, 52)
}

func (e *botConfigEditor) update(msg tea.KeyMsg) tea.Cmd {
	fields := e.fields()
	switch msg.String() {
	case "tab":
		fields[e.focus].Blur()
		e.focus = (e.focus + 1) % len(fields)
		return fields[e.focus].Focus()
	case "shift+tab":
		fields[e.focus].Blur()
		e.focus = (e.focus - 1 + len(fields)) % len(fields)
		return fields[e.focus].Focus()
	}
	before := fields[e.focus].Value()
	updated, cmd := fields[e.focus].Update(msg)
	*fields[e.focus] = updated
	if updated.Value() != before {
		e.formError = ""
		if e.focus == 1 {
			e.chatError = ""
		}
	}
	return cmd
}

func (e *botConfigEditor) request() (BotConfigRequest, error) {
	e.chatError = ""
	e.formError = ""
	chat, err := strconv.ParseInt(strings.TrimSpace(e.chat.Value()), 10, 64)
	if err != nil {
		e.chatError = "must be an integer"
		return BotConfigRequest{}, fmt.Errorf("chat ID must be an integer")
	}
	return BotConfigRequest{
		BotToken:        strings.TrimSpace(e.token.Value()),
		BotChat:         chat,
		BotControlToken: strings.TrimSpace(e.control.Value()),
	}, nil
}

func (e *botConfigEditor) dirty() bool {
	if e == nil {
		return false
	}
	return e.token.Value() != e.base.BotToken ||
		e.chat.Value() != strconv.FormatInt(e.base.BotChat, 10) ||
		e.control.Value() != e.base.BotControlToken
}

// rebase keeps locally edited fields and adopts remote values for untouched
// fields, then pins the next save to the latest ETag.
func (e *botConfigEditor) rebase(latest *BotConfigResponse) {
	if e == nil || latest == nil {
		return
	}
	if e.token.Value() == e.base.BotToken {
		e.token.SetValue(latest.BotToken)
	}
	if e.chat.Value() == strconv.FormatInt(e.base.BotChat, 10) {
		e.chat.SetValue(strconv.FormatInt(latest.BotChat, 10))
	}
	if e.control.Value() == e.base.BotControlToken {
		e.control.SetValue(latest.BotControlToken)
	}
	e.base = *latest
	e.etag = latest.ETag
	e.chatError = ""
	e.formError = ""
}
