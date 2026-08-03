package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudapp3/vmflow/internal/i18n"
)

// botConfigEditor edits the Telegram bot configuration. Token fields are masked
// via textinput.EchoPassword so secrets are not shown on screen.
type botConfigEditor struct {
	tr      *i18n.Translator
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
	return newBotConfigEditorWithTranslator(cfg, i18n.New(i18n.English))
}

func newBotConfigEditorWithTranslator(cfg *BotConfigResponse, translator *i18n.Translator) *botConfigEditor {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	token := textinput.New()
	token.Prompt = ""
	token.EchoMode = textinput.EchoPassword
	token.SetValue(cfg.BotToken)
	token.CharLimit = 256
	token.Width = 40
	token.Placeholder = translator.T("tui.bot_editor.placeholder.token")

	chat := textinput.New()
	chat.Prompt = ""
	chat.SetValue(strconv.FormatInt(cfg.BotChat, 10))
	chat.CharLimit = 20
	chat.Width = 40
	chat.Placeholder = translator.T("tui.bot_editor.placeholder.chat_id")

	control := textinput.New()
	control.Prompt = ""
	control.EchoMode = textinput.EchoPassword
	control.SetValue(cfg.BotControlToken)
	control.CharLimit = 256
	control.Width = 40
	control.Placeholder = translator.T("tui.bot_editor.placeholder.control_token")

	e := &botConfigEditor{tr: translator, token: token, chat: chat, control: control, etag: cfg.ETag, base: *cfg}
	e.token.Focus()
	return e
}

func (e *botConfigEditor) setTranslator(translator *i18n.Translator) {
	if e == nil {
		return
	}
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	previous := e.tr
	e.tr = translator
	e.token.Placeholder = translator.T("tui.bot_editor.placeholder.token")
	e.chat.Placeholder = translator.T("tui.bot_editor.placeholder.chat_id")
	e.control.Placeholder = translator.T("tui.bot_editor.placeholder.control_token")
	if e.chatError != "" {
		e.chatError = translator.T("tui.validation.integer")
	}
	if previous == nil || e.formError == "" {
		return
	}
	for _, key := range []string{
		"tui.status.bot_admin_required",
		"tui.status.bot_config_rejected",
		"tui.status.bot_latest_loaded_editor",
		"tui.status.bot_remote_changed_loading",
		"tui.status.latest_required_retry",
		"tui.status.load_latest_before_save",
		"tui.status.loading_latest_config",
	} {
		if e.formError == previous.T(key) {
			e.formError = translator.T(key)
			break
		}
	}
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
		e.chatError = e.tr.T("tui.validation.integer")
		return BotConfigRequest{}, fmt.Errorf("%s", e.tr.T("tui.validation.chat_id_integer"))
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
