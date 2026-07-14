package controlapi

import "errors"

// BotSettings is the Telegram bot configuration managed at runtime.
type BotSettings struct {
	Token        string
	ChatID       int64
	ControlToken string
}

// BotController manages the Telegram bot lifecycle at runtime, allowing the bot
// to be started, stopped, and reconfigured without restarting the daemon. It is
// implemented by the bot package and injected into Runtime so controlapi does
// not need to import bot (avoiding an import cycle).
type BotController interface {
	// Apply stops any running bot and starts a new one with settings. If Token
	// or ChatID are zero, the bot is stopped and not restarted.
	Apply(settings BotSettings) error
	// Stop stops the running bot without changing persisted configuration.
	Stop() error
	// Running reports whether a bot goroutine is currently active.
	Running() bool
}

// BotValidationError reports bot settings that Telegram has rejected. Error is
// intentionally generic because low-level HTTP errors may contain the bot token
// in their request URL.
type BotValidationError struct {
	Err error
}

func (err *BotValidationError) Error() string {
	return "telegram bot settings are invalid"
}

func (err *BotValidationError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// BotUnavailableError reports that readiness could not be established due to
// a transient Telegram or network failure.
type BotUnavailableError struct {
	Err error
}

func (err *BotUnavailableError) Error() string {
	return "telegram bot readiness check failed"
}

func (err *BotUnavailableError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func IsBotValidationError(err error) bool {
	var target *BotValidationError
	return errors.As(err, &target)
}
