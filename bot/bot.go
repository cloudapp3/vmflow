package bot

import (
	"context"
	"fmt"
	"log"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/tgbot/ext"
	"github.com/cloudapp3/vmflow/engine"
)

type Bot struct {
	bot     *tg.Bot
	app     *ext.Application
	manager *engine.Manager
	chatID  int64
}

func NewBot(token string, chatID int64, manager *engine.Manager) (*Bot, error) {
	b, err := tg.NewBot(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	app, err := ext.NewApplication(b,
		ext.WithContinueOnError(true),
		ext.WithErrorHandler(func(_ context.Context, _ *ext.Context, err error) {
			log.Printf("[bot] handler error: %v", err)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create bot app: %w", err)
	}

	bot := &Bot{
		bot:     b,
		app:     app,
		manager: manager,
		chatID:  chatID,
	}

	bot.registerHandlers()
	return bot, nil
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

func (b *Bot) registerHandlers() {
	b.app.AddHandler(ext.NewCommandHandler("start", b.handleStart))
	b.app.AddHandler(ext.NewCommandHandler("status", b.handleStatus))
	b.app.AddHandler(ext.NewCommandHandler("rules", b.handleRules))
	b.app.AddHandler(ext.NewCommandHandler("detail", b.handleDetail))
	b.app.AddHandler(ext.NewCommandHandler("reload", b.handleReload))
	b.app.AddHandler(ext.NewCommandHandler("stop", b.handleStop))
	b.app.AddHandler(ext.NewCommandHandler("start_rule", b.handleStartRule))
	b.app.AddHandler(ext.NewCallbackQueryHandler(nil, b.handleCallback))
}
