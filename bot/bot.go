package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/tgbot/ext"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

type Bot struct {
	bot     *tg.Bot
	app     *ext.Application
	manager *engine.Manager
	control *controlapi.Client
	chatID  int64
}

// NewBot creates a Telegram bot. manager powers read-only queries (/status,
// /rules, /detail). control is the control API client used for write actions
// (/reload, /stop, /start_rule, /toggle); when nil or without a token the bot
// runs in read-only mode and write commands report that control is disabled.
func NewBot(token string, chatID int64, manager *engine.Manager, control *controlapi.Client) (*Bot, error) {
	b, err := tg.NewBot(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	app, err := ext.NewApplication(b,
		ext.WithContinueOnError(true),
		ext.WithErrorHandler(func(_ context.Context, _ *ext.Context, err error) {
			log.Printf("[bot] handler error: %s", botHandlerErrorSummary(err))
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create bot app: %w", err)
	}

	bot := &Bot{
		bot:     b,
		app:     app,
		manager: manager,
		control: control,
		chatID:  chatID,
	}

	bot.registerHandlers()
	return bot, nil
}

// botHandlerErrorSummary keeps handler failures observable without rendering
// raw errors. Telegram transport errors can embed the bot token in the request
// URL, while control API errors may carry response bodies.
func botHandlerErrorSummary(err error) string {
	if err == nil {
		return "category=unknown"
	}
	if errors.Is(err, context.Canceled) {
		return "category=context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "category=deadline_exceeded"
	}

	var telegramErr *tg.APIError
	if errors.As(err, &telegramErr) {
		return fmt.Sprintf("category=telegram_api status=%d code=%d retry_after=%d",
			telegramErr.StatusCode, telegramErr.Code, telegramErr.RetryAfter)
	}
	var controlErr *controlapi.APIError
	if errors.As(err, &controlErr) {
		return fmt.Sprintf("category=control_api status=%d", controlErr.StatusCode)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return fmt.Sprintf("category=network timeout=%t", networkErr.Timeout())
	}
	return fmt.Sprintf("category=internal type=%T", err)
}

func (b *Bot) Start(ctx context.Context) error {
	log.Println("[bot] starting polling...")
	return b.app.RunPolling(ctx,
		ext.WithPollingAllowedUpdates(
			ext.UpdateTypeMessage,
			ext.UpdateTypeCallbackQuery,
		),
		ext.WithPollingNonBlockingDispatch(),
	)
}

// Ready verifies the token with Telegram before Manager replaces a running bot.
func (b *Bot) Ready(ctx context.Context) error {
	if b == nil || b.bot == nil {
		return fmt.Errorf("telegram bot is unavailable")
	}
	_, err := b.bot.GetMe(ctx, nil)
	return err
}

func (b *Bot) registerHandlers() {
	b.app.AddHandler(ext.NewCommandHandler("start", b.handleStart))
	b.app.AddHandler(ext.NewCommandHandler("status", b.handleStatus))
	b.app.AddHandler(ext.NewCommandHandler("rules", b.handleRules))
	b.app.AddHandler(ext.NewCommandHandler("detail", b.handleDetail))
	b.app.AddHandler(ext.NewCommandHandler("reload", b.handleReload))
	b.app.AddHandler(ext.NewCommandHandler("stop", b.handleStop))
	b.app.AddHandler(ext.NewCommandHandler("start_rule", b.handleStartRule))
	b.app.AddHandler(ext.NewCommandHandler("toggle", b.handleToggle))
	b.app.AddHandler(ext.NewCallbackQueryHandler(nil, b.handleCallback))
}
