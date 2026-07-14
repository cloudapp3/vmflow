package bot

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/tgbot/ext"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

// ── Auth Check ─────────────────────────────────────────────────────

func (b *Bot) chatAllowed(c *ext.Context) bool {
	if c == nil || c.Update == nil {
		return false
	}
	if query := c.Update.CallbackQuery; query != nil {
		target, ok := callbackTargetFromQuery(query)
		return ok && target.chatID == b.chatID
	}
	msg := c.EffectiveMessage()
	if msg != nil && msg.Chat != nil && msg.Chat.ID == b.chatID {
		return true
	}
	return false
}

func (b *Bot) rejectChat(ctx context.Context, c *ext.Context) error {
	msg := c.EffectiveMessage()
	if msg == nil || msg.Chat == nil {
		return nil
	}
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   "⛔ Unauthorized",
	})
	return err
}

// canControl reports whether the bot may perform write actions via the control
// API. Requires a control client with an admin token.
func (b *Bot) canControl() bool {
	return b.control != nil && b.control.HasToken()
}

// reply sends a plain text message to the chat of the incoming context.
func (b *Bot) reply(ctx context.Context, c *ext.Context, text string) error {
	msg := c.EffectiveMessage()
	if msg == nil || msg.Chat == nil {
		return nil
	}
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{ChatID: msg.Chat.ID, Text: text})
	return err
}

const (
	controlDisabledMsg    = "⛔ 控制未启用(需配置 bot_control_token 为 admin 令牌)"
	telegramTextLimit     = 4096
	callbackDataByteLimit = 64
	ruleSetCallbackPrefix = "rule:set:"
)

// ── /start ─────────────────────────────────────────────────────────

func (b *Bot) handleStart(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	text := `*vmflow* - L4 Port Forwarding Engine

Commands:
/status - View running status
/rules - List all rules
/detail <id> - Rule detail
/reload - Reload configuration from disk
/stop <id> - Disable a rule (persists to config)
/start_rule <id> - Enable a rule (persists to config)
/toggle <id> - Toggle a rule

Write commands require bot_control_token (admin).`

	msg := c.EffectiveMessage()
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      text,
		ParseMode: "Markdown",
	})
	return err
}

// ── /status ────────────────────────────────────────────────────────

func (b *Bot) handleStatus(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	rules := b.manager.RunningRules()
	snapshots := b.manager.SnapshotAll()

	var totalUp, totalDown int64
	var totalConns int64
	statsMap := make(map[string]engine.TrafficSnapshot, len(snapshots))
	for _, s := range snapshots {
		statsMap[s.RuleID] = s
		totalUp += s.UploadBytes
		totalDown += s.DownloadBytes
		totalConns += s.Conns
	}

	text := fmt.Sprintf(
		"*vmflow Status*\n\nRunning: %d rules\nConnections: %d\nUpload: %s\nDownload: %s",
		len(rules),
		totalConns,
		formatBytes(totalUp),
		formatBytes(totalDown),
	)

	msg := c.EffectiveMessage()
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      text,
		ParseMode: "Markdown",
	})
	return err
}

// ── /rules ─────────────────────────────────────────────────────────

func (b *Bot) handleRules(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	rules := b.manager.RunningRules()
	snapshots := b.manager.SnapshotAll()
	statsMap := make(map[string]engine.TrafficSnapshot, len(snapshots))
	for _, s := range snapshots {
		statsMap[s.RuleID] = s
	}

	if len(rules) == 0 {
		msg := c.EffectiveMessage()
		_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "No running rules.",
		})
		return err
	}

	msg := c.EffectiveMessage()
	return b.sendRuleMessages(ctx, msg.Chat.ID, buildRuleMessages(rules, statsMap))
}

// ── /detail ────────────────────────────────────────────────────────

func (b *Bot) handleDetail(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		return b.reply(ctx, c, "Usage: /detail <rule_id>")
	}

	rules := b.manager.RunningRules()
	var found *engine.Rule
	for i := range rules {
		if rules[i].RuleID == ruleID {
			found = &rules[i]
			break
		}
	}
	if found == nil {
		return b.reply(ctx, c, fmt.Sprintf("Rule not found: %s", ruleID))
	}

	snap := b.manager.Snapshot(ruleID)
	text := fmt.Sprintf(
		"*Rule Detail*\n\n"+
			"ID: `%s`\n"+
			"Name: %s\n"+
			"Protocol: %s\n"+
			"Listen: %s:%d\n"+
			"Target: %s:%d\n"+
			"Speed Limit: %s\n"+
			"Max Conns: %s\n\n"+
			"*Traffic*\n"+
			"Upload: %s\n"+
			"Download: %s\n"+
			"Connections: %d",
		found.RuleID,
		found.Name,
		strings.ToUpper(string(found.Protocol)),
		found.ListenAddr, found.ListenPort,
		found.TargetAddr, found.TargetPort,
		speedLimitStr(found.SpeedLimit),
		maxConnStr(found.MaxConn),
		formatBytes(snap.UploadBytes),
		formatBytes(snap.DownloadBytes),
		snap.Conns,
	)

	msg := c.EffectiveMessage()
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      text,
		ParseMode: "Markdown",
	})
	return err
}

// ── /reload (with confirmation) ────────────────────────────────────

func (b *Bot) handleReload(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}
	if !b.canControl() {
		return b.reply(ctx, c, controlDisabledMsg)
	}

	msg := c.EffectiveMessage()
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   "⚠️ 从磁盘重载配置?",
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ 确认", CallbackData: "reload:confirm"},
					{Text: "❌ 取消", CallbackData: "reload:cancel"},
				},
			},
		},
	})
	return err
}

// ── /stop (with confirmation) ──────────────────────────────────────

func (b *Bot) handleStop(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}
	if !b.canControl() {
		return b.reply(ctx, c, controlDisabledMsg)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		return b.reply(ctx, c, "Usage: /stop <rule_id>")
	}
	confirmData := ruleSetCallbackData(ruleID, false)

	msg := c.EffectiveMessage()
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   fmt.Sprintf("⚠️ 停用规则 %s?(将写入配置)", ruleID),
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ 确认", CallbackData: confirmData},
					{Text: "❌ 取消", CallbackData: "rule:cancel"},
				},
			},
		},
	})
	return err
}

// ── /start_rule (with confirmation) ────────────────────────────────

func (b *Bot) handleStartRule(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}
	if !b.canControl() {
		return b.reply(ctx, c, controlDisabledMsg)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		return b.reply(ctx, c, "Usage: /start_rule <rule_id>")
	}
	confirmData := ruleSetCallbackData(ruleID, true)

	msg := c.EffectiveMessage()
	_, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   fmt.Sprintf("⚠️ 启用规则 %s?(将写入配置)", ruleID),
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ 确认", CallbackData: confirmData},
					{Text: "❌ 取消", CallbackData: "rule:cancel"},
				},
			},
		},
	})
	return err
}

// ── /toggle (with confirmation) ────────────────────────────────────

func (b *Bot) handleToggle(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}
	if !b.canControl() {
		return b.reply(ctx, c, controlDisabledMsg)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		return b.reply(ctx, c, "Usage: /toggle <rule_id>")
	}

	cfg, err := b.control.ConfigRules(ctx)
	if err != nil {
		return b.reply(ctx, c, "❌ 读取配置失败:"+controlErrorMessage(err))
	}
	targetEnabled, found := false, false
	for i := range cfg.Rules {
		if cfg.Rules[i].RuleID == ruleID {
			targetEnabled = !cfg.Rules[i].Enabled
			found = true
			break
		}
	}
	if !found {
		return b.reply(ctx, c, fmt.Sprintf("❌ 规则不存在:%s", ruleID))
	}
	action := "停用"
	if targetEnabled {
		action = "启用"
	}
	confirmData := ruleSetCallbackData(ruleID, targetEnabled)

	msg := c.EffectiveMessage()
	_, err = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   fmt.Sprintf("⚠️ %s规则 %s?(将写入配置)", action, ruleID),
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ 确认", CallbackData: confirmData},
					{Text: "❌ 取消", CallbackData: "rule:cancel"},
				},
			},
		},
	})
	return err
}

// ── Callback Query Handler ─────────────────────────────────────────

func (b *Bot) handleCallback(ctx context.Context, c *ext.Context) error {
	if c == nil || c.Update == nil || c.Update.CallbackQuery == nil {
		return nil
	}
	query := c.Update.CallbackQuery
	target, hasTarget := callbackTargetFromQuery(query)
	if !b.chatAllowed(c) {
		_, err := b.bot.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "⛔ Unauthorized",
			ShowAlert:       true,
		})
		return err
	}

	data := query.Data

	var resultText string
	if enabled, token, ok := parseRuleSetCallbackData(data); ok {
		resultText = b.setRuleTokenEnabled(ctx, token, enabled)
	} else {
		switch {
		case data == "reload:confirm":
			resultText = b.doReload(ctx)
		case data == "reload:cancel", data == "rule:cancel":
			resultText = "已取消操作。"
		// Accept idempotent callbacks emitted by the previous release. Old toggle
		// callbacks are deliberately expired because they encode no target state.
		case strings.HasPrefix(data, "stop:confirm:"):
			resultText = b.doStopRule(ctx, strings.TrimPrefix(data, "stop:confirm:"))
		case strings.HasPrefix(data, "stop:cancel:"):
			resultText = "已取消 stop。"
		case strings.HasPrefix(data, "start:confirm:"):
			resultText = b.doStartRule(ctx, strings.TrimPrefix(data, "start:confirm:"))
		case strings.HasPrefix(data, "start:cancel:"):
			resultText = "已取消 start。"
		case strings.HasPrefix(data, "toggle:confirm:"):
			resultText = "旧版 toggle 确认已失效,请重新执行 /toggle。"
		case strings.HasPrefix(data, "toggle:cancel:"):
			resultText = "已取消 toggle。"
		default:
			resultText = "未知操作。"
		}
	}

	_, answerErr := b.bot.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            truncateUTF8(resultText, 200),
	})

	var editErr error
	if target.editable {
		_, editErr = b.bot.EditMessageReplyMarkup(ctx, &tg.EditMessageReplyMarkupParams{
			ChatID:      target.chatID,
			MessageID:   target.messageID,
			ReplyMarkup: tg.InlineKeyboardMarkup{},
		})
	}

	_, sendErr := b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:          target.chatID,
		MessageThreadID: target.messageThreadID,
		Text:            resultText,
	})
	if !hasTarget {
		return errors.Join(answerErr, editErr, fmt.Errorf("callback has no chat target"))
	}
	return errors.Join(answerErr, editErr, sendErr)
}

type callbackTarget struct {
	chatID          int64
	messageID       int64
	messageThreadID int64
	editable        bool
}

func callbackTargetFromQuery(query *tg.CallbackQuery) (callbackTarget, bool) {
	if query == nil {
		return callbackTarget{}, false
	}
	switch message := query.Message.(type) {
	case *tg.Message:
		if message == nil || message.Chat == nil {
			return callbackTarget{}, false
		}
		return callbackTarget{
			chatID:          message.Chat.ID,
			messageID:       message.MessageID,
			messageThreadID: message.MessageThreadID,
			editable:        true,
		}, true
	case *tg.InaccessibleMessage:
		if message == nil || message.Chat == nil {
			return callbackTarget{}, false
		}
		return callbackTarget{chatID: message.Chat.ID, messageID: message.MessageID}, true
	default:
		// Inline callbacks have no trustworthy chat ID. The configured chat is
		// the authorization boundary, so they cannot be authorized safely.
		return callbackTarget{}, false
	}
}

func ruleSetCallbackData(ruleID string, enabled bool) string {
	state := "0"
	if enabled {
		state = "1"
	}
	data := ruleSetCallbackPrefix + state + ":" + ruleCallbackToken(ruleID)
	if len(data) > callbackDataByteLimit {
		panic("rule callback data exceeds Telegram limit")
	}
	return data
}

func parseRuleSetCallbackData(data string) (enabled bool, token string, ok bool) {
	if !strings.HasPrefix(data, ruleSetCallbackPrefix) {
		return false, "", false
	}
	rest := strings.TrimPrefix(data, ruleSetCallbackPrefix)
	state, token, found := strings.Cut(rest, ":")
	if !found || (state != "0" && state != "1") || len(token) != 43 {
		return false, "", false
	}
	digest, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(digest) != sha256.Size {
		return false, "", false
	}
	return state == "1", token, true
}

func ruleCallbackToken(ruleID string) string {
	digest := sha256.Sum256([]byte(ruleID))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// ── Action Executors ───────────────────────────────────────────────

func (b *Bot) doReload(ctx context.Context) string {
	if !b.canControl() {
		return controlDisabledMsg
	}
	resp, err := b.control.Reload(ctx)
	if err != nil {
		return "❌ reload 失败:" + controlErrorMessage(err)
	}
	return fmt.Sprintf("✅ reload 完成,共 %d 条规则", resp.RuleCount)
}

func (b *Bot) doStopRule(ctx context.Context, ruleID string) string {
	return b.setRuleEnabled(ctx, ruleID, false)
}

func (b *Bot) doStartRule(ctx context.Context, ruleID string) string {
	return b.setRuleEnabled(ctx, ruleID, true)
}

func (b *Bot) doToggleRule(ctx context.Context, ruleID string) string {
	if !b.canControl() {
		return controlDisabledMsg
	}
	cfg, err := b.control.ConfigRules(ctx)
	if err != nil {
		return "❌ 读取配置失败:" + controlErrorMessage(err)
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].RuleID == ruleID {
			return b.applyEnabled(ctx, cfg, ruleID, !cfg.Rules[i].Enabled)
		}
	}
	return fmt.Sprintf("❌ 规则不存在:%s", ruleID)
}

// setRuleEnabled fetches the config, sets one rule's enabled flag, and applies
// it back via If-Match (persistent: writes to config.yaml + hot-applies).
func (b *Bot) setRuleEnabled(ctx context.Context, ruleID string, enabled bool) string {
	if !b.canControl() {
		return controlDisabledMsg
	}
	cfg, err := b.control.ConfigRules(ctx)
	if err != nil {
		return "❌ 读取配置失败:" + controlErrorMessage(err)
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].RuleID == ruleID {
			return b.setRuleEnabledInConfig(ctx, cfg, i, enabled)
		}
	}
	return fmt.Sprintf("❌ 规则不存在:%s", ruleID)
}

// setRuleTokenEnabled resolves a full SHA-256 token against the current
// configuration. Ambiguous tokens are rejected rather than risking an update
// to the wrong rule.
func (b *Bot) setRuleTokenEnabled(ctx context.Context, token string, enabled bool) string {
	if !b.canControl() {
		return controlDisabledMsg
	}
	cfg, err := b.control.ConfigRules(ctx)
	if err != nil {
		return "❌ 读取配置失败:" + controlErrorMessage(err)
	}
	match := -1
	for i := range cfg.Rules {
		if ruleCallbackToken(cfg.Rules[i].RuleID) != token {
			continue
		}
		if match >= 0 {
			return "❌ 规则确认令牌不唯一,操作已拒绝。"
		}
		match = i
	}
	if match < 0 {
		return "❌ 规则不存在或确认已失效,请重新执行命令。"
	}
	return b.setRuleEnabledInConfig(ctx, cfg, match, enabled)
}

func (b *Bot) setRuleEnabledInConfig(ctx context.Context, cfg *controlapi.ConfigRulesResponse, index int, enabled bool) string {
	ruleID := cfg.Rules[index].RuleID
	if cfg.Rules[index].Enabled == enabled {
		state := "已启用"
		if !enabled {
			state = "已停用"
		}
		return fmt.Sprintf("ℹ️ 规则 %s 已是%s状态", ruleID, state)
	}
	cfg.Rules[index].Enabled = enabled
	return b.applyEnabled(ctx, cfg, ruleID, enabled)
}

// applyEnabled writes the modified config back with If-Match and reports.
func (b *Bot) applyEnabled(ctx context.Context, cfg *controlapi.ConfigRulesResponse, ruleID string, enabled bool) string {
	draft := controlapi.ConfigRulesRequest{UDPMaxSessions: cfg.UDPMaxSessions, Rules: cfg.Rules}
	if _, err := b.control.Apply(ctx, cfg.ETag, draft); err != nil {
		action := "启用"
		if !enabled {
			action = "停用"
		}
		return fmt.Sprintf("❌ %s失败:%s", action, controlErrorMessage(err))
	}
	state := "已启用"
	if !enabled {
		state = "已停用"
	}
	return fmt.Sprintf("✅ 规则 %s %s(已写入配置)", ruleID, state)
}

// controlErrorMessage maps control API errors to user-facing hints.
func controlErrorMessage(err error) string {
	switch controlapi.APIStatus(err) {
	case http.StatusPreconditionFailed:
		return "配置已被其他途径修改,请重试"
	case http.StatusForbidden:
		return "权限不足(bot_control_token 需为 admin)"
	case http.StatusUnprocessableEntity:
		return "precheck 失败(端口冲突或配置非法)"
	case http.StatusServiceUnavailable:
		return "应用失败,运行时已回滚"
	default:
		return "操作失败,请查看 daemon 日志"
	}
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

func speedLimitStr(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return formatBytes(n) + "/s"
}

func maxConnStr(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}

func buildRuleMessages(rules []engine.Rule, statsMap map[string]engine.TrafficSnapshot) []string {
	header := "Forwarding Rules\n\n" + fmt.Sprintf("%-14s %-6s %-22s %6s %10s %10s\n",
		"Name", "Proto", "Listen->Target", "Conns", "Upload", "Download") +
		strings.Repeat("-", 73) + "\n"

	chunks := make([]string, 0, 1)
	current := header
	for _, rule := range rules {
		stats := statsMap[rule.RuleID]
		listen := fmt.Sprintf("%s:%d", rule.ListenAddr, rule.ListenPort)
		target := fmt.Sprintf("%s:%d", rule.TargetAddr, rule.TargetPort)
		addr := truncateRunes(listen+"->"+target, 22)
		line := fmt.Sprintf("%-14s %-6s %-22s %6d %10s %10s\n",
			truncateRunes(rule.Name, 14),
			truncateRunes(strings.ToUpper(string(rule.Protocol)), 6),
			addr,
			stats.Conns,
			formatBytes(stats.UploadBytes),
			formatBytes(stats.DownloadBytes),
		)
		if len(current)+len(line) > telegramTextLimit {
			chunks = append(chunks, strings.TrimSuffix(current, "\n"))
			current = header
		}
		current += line
	}
	if current != header || len(chunks) == 0 {
		chunks = append(chunks, strings.TrimSuffix(current, "\n"))
	}
	return chunks
}

func (b *Bot) sendRuleMessages(ctx context.Context, chatID int64, messages []string) error {
	for i, text := range messages {
		if len(text) > telegramTextLimit {
			return fmt.Errorf("rules chunk %d exceeds Telegram text limit", i+1)
		}
		if _, err := b.bot.SendMessage(ctx, &tg.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
			return fmt.Errorf("send rules chunk %d/%d: %w", i+1, len(messages), err)
		}
	}
	return nil
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	if limit <= 2 {
		return string([]rune(value)[:limit])
	}
	return string([]rune(value)[:limit-2]) + ".."
}

// truncateUTF8 limits a Telegram field by bytes without splitting a rune.
func truncateUTF8(value string, byteLimit int) string {
	if byteLimit <= 0 {
		return ""
	}
	if len(value) <= byteLimit {
		return value
	}
	for byteLimit > 0 && !utf8.RuneStart(value[byteLimit]) {
		byteLimit--
	}
	return value[:byteLimit]
}
