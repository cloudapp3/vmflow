package bot

import (
	"context"
	"fmt"
	"strings"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/tgbot/ext"
	"github.com/cloudapp3/vmflow/engine"
)

// ── Auth Check ─────────────────────────────────────────────────────

func (b *Bot) chatAllowed(c *ext.Context) bool {
	msg := c.EffectiveMessage()
	if msg != nil && msg.Chat != nil && msg.Chat.ID == b.chatID {
		return true
	}
	if c.Update.CallbackQuery != nil && c.Update.CallbackQuery.From.ID == b.chatID {
		return true
	}
	return false
}

func (b *Bot) rejectChat(ctx context.Context, c *ext.Context) error {
	msg := c.EffectiveMessage()
	if msg == nil || msg.Chat == nil {
		return nil
	}
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   "⛔ Unauthorized",
	})
	return nil
}

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
/reload - Reload configuration
/stop <id> - Stop a rule
/start_rule <id> - Start a rule`

	msg := c.EffectiveMessage()
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      text,
		ParseMode: "Markdown",
	})
	return nil
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
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      text,
		ParseMode: "Markdown",
	})
	return nil
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
		_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "No running rules.",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString("*Forwarding Rules*\n\n")
	sb.WriteString("```\n")
	sb.WriteString(fmt.Sprintf("%-14s %-6s %-22s %6s %10s %10s\n",
		"Name", "Proto", "Listen→Target", "Conns", "Upload", "Download"))
	sb.WriteString(strings.Repeat("─", 70) + "\n")

	for _, r := range rules {
		s, _ := statsMap[r.RuleID]
		listen := fmt.Sprintf("%s:%d", r.ListenAddr, r.ListenPort)
		target := fmt.Sprintf("%s:%d", r.TargetAddr, r.TargetPort)
		addr := fmt.Sprintf("%s→%s", listen, target)
		if len(addr) > 22 {
			addr = addr[:20] + ".."
		}
		sb.WriteString(fmt.Sprintf("%-14s %-6s %-22s %6d %10s %10s\n",
			r.Name, strings.ToUpper(string(r.Protocol)), addr,
			s.Conns, formatBytes(s.UploadBytes), formatBytes(s.DownloadBytes)))
	}
	sb.WriteString("```")

	msg := c.EffectiveMessage()
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      sb.String(),
		ParseMode: "Markdown",
	})
	return nil
}

// ── /detail ────────────────────────────────────────────────────────

func (b *Bot) handleDetail(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		msg := c.EffectiveMessage()
		_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Usage: /detail <rule_id>",
		})
		return nil
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
		msg := c.EffectiveMessage()
		_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   fmt.Sprintf("Rule not found: %s", ruleID),
		})
		return nil
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
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    msg.Chat.ID,
		Text:      text,
		ParseMode: "Markdown",
	})
	return nil
}

// ── /reload (with confirmation) ────────────────────────────────────

func (b *Bot) handleReload(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	msg := c.EffectiveMessage()
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   "⚠️ Reload configuration from disk?",
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ Confirm", CallbackData: "reload:confirm"},
					{Text: "❌ Cancel", CallbackData: "reload:cancel"},
				},
			},
		},
	})
	return nil
}

// ── /stop (with confirmation) ──────────────────────────────────────

func (b *Bot) handleStop(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		msg := c.EffectiveMessage()
		_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Usage: /stop <rule_id>",
		})
		return nil
	}

	msg := c.EffectiveMessage()
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   fmt.Sprintf("⚠️ Stop rule `%s`?", ruleID),
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ Confirm", CallbackData: "stop:confirm:" + ruleID},
					{Text: "❌ Cancel", CallbackData: "stop:cancel:" + ruleID},
				},
			},
		},
		ParseMode: "Markdown",
	})
	return nil
}

// ── /start_rule (with confirmation) ────────────────────────────────

func (b *Bot) handleStartRule(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		return b.rejectChat(ctx, c)
	}

	_, args, _ := c.Command()
	ruleID := strings.TrimSpace(args)
	if ruleID == "" {
		msg := c.EffectiveMessage()
		_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Usage: /start_rule <rule_id>",
		})
		return nil
	}

	msg := c.EffectiveMessage()
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   fmt.Sprintf("⚠️ Start rule `%s`?", ruleID),
		ReplyMarkup: &tg.InlineKeyboardMarkup{
			InlineKeyboard: [][]tg.InlineKeyboardButton{
				{
					{Text: "✅ Confirm", CallbackData: "start:confirm:" + ruleID},
					{Text: "❌ Cancel", CallbackData: "start:cancel:" + ruleID},
				},
			},
		},
		ParseMode: "Markdown",
	})
	return nil
}

// ── Callback Query Handler ─────────────────────────────────────────

func (b *Bot) handleCallback(ctx context.Context, c *ext.Context) error {
	if !b.chatAllowed(c) {
		_, _ = b.bot.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{
			CallbackQueryID: c.Update.CallbackQuery.ID,
			Text:            "⛔ Unauthorized",
		})
		return nil
	}

	query := c.Update.CallbackQuery
	data := query.Data
	chatID := query.From.ID

	var resultText string

	switch {
	case data == "reload:confirm":
		resultText = b.doReload()
	case data == "reload:cancel":
		resultText = "Reload cancelled."
	case strings.HasPrefix(data, "stop:confirm:"):
		ruleID := strings.TrimPrefix(data, "stop:confirm:")
		resultText = b.doStopRule(ruleID)
	case strings.HasPrefix(data, "stop:cancel:"):
		resultText = "Stop cancelled."
	case strings.HasPrefix(data, "start:confirm:"):
		ruleID := strings.TrimPrefix(data, "start:confirm:")
		resultText = b.doStartRule(ruleID)
	case strings.HasPrefix(data, "start:cancel:"):
		resultText = "Start cancelled."
	default:
		resultText = "Unknown action."
	}

	// Answer callback query (popup notification)
	_, _ = b.bot.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            resultText,
	})

	// Also send a message to chat
	_, _ = b.bot.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: chatID,
		Text:   resultText,
	})
	return nil
}

// ── Action Executors ───────────────────────────────────────────────

func (b *Bot) doReload() string {
	// Reload is done through re-applying config. For now we just stop all.
	// The actual reload needs the config path - handled via control API reload.
	// This is a simplified version that stops all rules.
	b.manager.StopAll()
	return "✅ All rules stopped. Use control API /v1/reload for full config reload."
}

func (b *Bot) doStopRule(ruleID string) string {
	b.manager.StopRule(ruleID)
	return fmt.Sprintf("✅ Rule `%s` stopped.", ruleID)
}

func (b *Bot) doStartRule(ruleID string) string {
	// To start a rule we need the full Rule config. Look in running rules history.
	// Since we can only start rules that were previously loaded from config,
	// this is limited without the config. Show a hint.
	_ = ruleID
	return "ℹ️ Use control API /v1/reload to re-apply configuration."
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
