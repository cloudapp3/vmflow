package bot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

type fakeBotRunner struct {
	readyErr  error
	startErr  error
	started   chan struct{}
	stopped   chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

type contextReadyBotRunner struct {
	readyDone chan struct{}
}

func (runner *contextReadyBotRunner) Ready(ctx context.Context) error {
	<-ctx.Done()
	close(runner.readyDone)
	return ctx.Err()
}

func (*contextReadyBotRunner) Start(context.Context) error {
	return errors.New("unexpected start")
}

func (runner *fakeBotRunner) Ready(context.Context) error {
	return runner.readyErr
}

func (runner *fakeBotRunner) Start(ctx context.Context) error {
	if runner.started != nil {
		runner.startOnce.Do(func() { close(runner.started) })
	}
	if runner.startErr != nil {
		return runner.startErr
	}
	<-ctx.Done()
	if runner.stopped != nil {
		runner.stopOnce.Do(func() { close(runner.stopped) })
	}
	return ctx.Err()
}

func newTestManager() *Manager {
	return NewManager(engine.NewManager(engine.NewCollector()), nil, slog.Default())
}

func TestManagerNotRunningByDefault(t *testing.T) {
	mgr := newTestManager()
	if mgr.Running() {
		t.Fatalf("new manager should not be running")
	}
}

func TestManagerApplyEmptySettingsDisablesBot(t *testing.T) {
	mgr := newTestManager()
	if err := mgr.Apply(controlapi.BotSettings{}); err != nil {
		t.Fatalf("apply empty settings: %v", err)
	}
	if mgr.Running() {
		t.Fatalf("bot should not run with empty token/chat")
	}
}

func TestManagerStopClearsRunning(t *testing.T) {
	mgr := newTestManager()
	// Simulate a running bot without starting a real Telegram connection.
	mgr.running = true
	if err := mgr.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if mgr.Running() {
		t.Fatalf("bot should not be running after stop")
	}
}

func TestManagerStopIsIdempotent(t *testing.T) {
	mgr := newTestManager()
	if err := mgr.Stop(); err != nil {
		t.Fatalf("stop on idle manager: %v", err)
	}
	if err := mgr.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if mgr.Running() {
		t.Fatalf("bot should not be running after repeated stops")
	}
}

func TestManagerReadinessFailurePreservesRunningBot(t *testing.T) {
	oldRunner := &fakeBotRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	invalidRunner := &fakeBotRunner{readyErr: &tg.APIError{StatusCode: http.StatusUnauthorized, Code: http.StatusUnauthorized}}
	runners := []botRunner{oldRunner, invalidRunner}
	mgr := newTestManager()
	mgr.newBot = func(string, int64, *engine.Manager, *controlapi.Client) (botRunner, error) {
		runner := runners[0]
		runners = runners[1:]
		return runner, nil
	}

	if err := mgr.Apply(controlapi.BotSettings{Token: "old", ChatID: 1}); err != nil {
		t.Fatalf("apply old: %v", err)
	}
	select {
	case <-oldRunner.started:
	case <-time.After(time.Second):
		t.Fatal("old bot did not start")
	}
	err := mgr.Apply(controlapi.BotSettings{Token: "invalid", ChatID: 2})
	var validationErr *controlapi.BotValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("apply invalid error = %v, want BotValidationError", err)
	}
	if !mgr.Running() {
		t.Fatal("readiness failure stopped the old bot")
	}
	select {
	case <-oldRunner.stopped:
		t.Fatal("old bot was cancelled by failed candidate")
	default:
	}
	if err := mgr.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerInvalidTokenNeverReportsRunning(t *testing.T) {
	mgr := newTestManager()
	mgr.newBot = func(string, int64, *engine.Manager, *controlapi.Client) (botRunner, error) {
		return &fakeBotRunner{readyErr: &tg.APIError{StatusCode: http.StatusUnauthorized}}, nil
	}
	err := mgr.Apply(controlapi.BotSettings{Token: "invalid", ChatID: 1})
	var validationErr *controlapi.BotValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("apply error = %v, want BotValidationError", err)
	}
	if mgr.Running() {
		t.Fatal("invalid token reported running")
	}
}

func TestManagerClearsRunningWhenPollingExits(t *testing.T) {
	runner := &fakeBotRunner{started: make(chan struct{}), startErr: errors.New("polling failed")}
	mgr := newTestManager()
	mgr.newBot = func(string, int64, *engine.Manager, *controlapi.Client) (botRunner, error) {
		return runner, nil
	}
	if err := mgr.Apply(controlapi.BotSettings{Token: "valid", ChatID: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("polling did not start")
	}
	deadline := time.Now().Add(time.Second)
	for mgr.Running() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if mgr.Running() {
		t.Fatal("manager remained running after polling exited")
	}
}

func TestManagerWhitespaceTokenDisablesWithoutBuildingCandidate(t *testing.T) {
	mgr := newTestManager()
	called := false
	mgr.newBot = func(string, int64, *engine.Manager, *controlapi.Client) (botRunner, error) {
		called = true
		return nil, errors.New("unexpected")
	}
	mgr.running = true
	if err := mgr.Apply(controlapi.BotSettings{Token: " \t ", ChatID: 1}); err != nil {
		t.Fatal(err)
	}
	if called || mgr.Running() {
		t.Fatalf("whitespace token: factory_called=%v running=%v", called, mgr.Running())
	}
}

func TestManagerReadinessUsesBoundedContext(t *testing.T) {
	runner := &contextReadyBotRunner{readyDone: make(chan struct{})}
	mgr := newTestManager()
	mgr.readyFor = 10 * time.Millisecond
	mgr.newBot = func(string, int64, *engine.Manager, *controlapi.Client) (botRunner, error) {
		return runner, nil
	}
	started := time.Now()
	err := mgr.Apply(controlapi.BotSettings{Token: "unreachable", ChatID: 1})
	var unavailableErr *controlapi.BotUnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("apply error = %v, want BotUnavailableError", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("readiness cancellation took %v", elapsed)
	}
	select {
	case <-runner.readyDone:
	default:
		t.Fatal("readiness context was not cancelled")
	}
	if mgr.Running() {
		t.Fatal("timed-out readiness reported running")
	}
}
