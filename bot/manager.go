package bot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

// Keep readiness below controlapi.Client's five-second request timeout so a
// config PUT cannot time out at the caller and then commit successfully later.
const defaultBotReadinessTimeout = 3 * time.Second

type botRunner interface {
	Ready(context.Context) error
	Start(context.Context) error
}

type botFactory func(string, int64, *engine.Manager, *controlapi.Client) (botRunner, error)

// Manager implements controlapi.BotController, owning the Telegram bot goroutine
// lifecycle so the bot can be reconfigured at runtime without restarting the
// daemon.
type Manager struct {
	mu         sync.Mutex
	manager    *engine.Manager
	controlFn  func(controlToken string) *controlapi.Client
	logger     *slog.Logger
	newBot     botFactory
	readyFor   time.Duration
	bot        botRunner
	cancel     context.CancelFunc
	running    bool
	generation uint64
}

// NewManager creates a bot lifecycle manager. controlFn builds a control API
// client for a given control token (used by the bot for write actions); it may
// return nil when no token is configured, in which case the bot runs read-only.
func NewManager(manager *engine.Manager, controlFn func(string) *controlapi.Client, logger *slog.Logger) *Manager {
	return &Manager{
		manager:   manager,
		controlFn: controlFn,
		logger:    logger,
		newBot: func(token string, chatID int64, manager *engine.Manager, client *controlapi.Client) (botRunner, error) {
			return NewBot(token, chatID, manager, client)
		},
		readyFor: defaultBotReadinessTimeout,
	}
}

// Apply stops any running bot and starts a new one with settings. With an empty
// token or chat ID the bot is stopped and not restarted.
func (m *Manager) Apply(settings controlapi.BotSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings.Token = strings.TrimSpace(settings.Token)
	settings.ControlToken = strings.TrimSpace(settings.ControlToken)
	if settings.Token == "" || settings.ChatID == 0 {
		m.stopLocked()
		if m.logger != nil {
			m.logger.Info("bot not started (no token or chat)", "component", "bot", "event", "bot_disabled")
		}
		return nil
	}
	var client *controlapi.Client
	if m.controlFn != nil {
		client = m.controlFn(settings.ControlToken)
	}
	if m.newBot == nil {
		return &controlapi.BotUnavailableError{Err: errors.New("bot factory is unavailable")}
	}
	b, err := m.newBot(settings.Token, settings.ChatID, m.manager, client)
	if err != nil {
		return &controlapi.BotValidationError{Err: err}
	}
	if b == nil {
		return &controlapi.BotUnavailableError{Err: errors.New("bot factory returned no candidate")}
	}
	readyFor := m.readyFor
	if readyFor <= 0 {
		readyFor = defaultBotReadinessTimeout
	}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), readyFor)
	err = b.Ready(readyCtx)
	readyCancel()
	if err != nil {
		return classifyReadinessError(err)
	}

	// The candidate is fully constructed and authenticated before the old bot is
	// cancelled, so failed updates leave the current bot untouched.
	m.stopLocked()
	ctx, cancel := context.WithCancel(context.Background())
	m.generation++
	generation := m.generation
	m.bot = b
	m.cancel = cancel
	m.running = true
	go func() {
		runErr := b.Start(ctx)
		m.mu.Lock()
		if m.generation == generation {
			m.bot = nil
			m.cancel = nil
			m.running = false
		}
		m.mu.Unlock()
		if runErr != nil && !errors.Is(runErr, context.Canceled) && m.logger != nil {
			m.logger.Warn("bot stopped", "component", "bot", "event", "stopped", "error", "telegram polling stopped unexpectedly")
		}
	}()
	if m.logger != nil {
		m.logger.Info("telegram bot started", "component", "bot", "event", "started", "control_enabled", client != nil && client.HasToken())
	}
	return nil
}

// Stop stops the running bot without changing persisted configuration.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	wasRunning := m.running
	m.stopLocked()
	if wasRunning && m.logger != nil {
		m.logger.Info("bot stopped", "component", "bot", "event", "bot_stopped")
	}
	return nil
}

// Running reports whether a bot goroutine is currently active.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Manager) stopLocked() {
	m.generation++
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.bot = nil
	m.running = false
}

func classifyReadinessError(err error) error {
	var apiErr *tg.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return &controlapi.BotValidationError{Err: err}
		}
		switch apiErr.Code {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return &controlapi.BotValidationError{Err: err}
		}
	}
	return &controlapi.BotUnavailableError{Err: err}
}
