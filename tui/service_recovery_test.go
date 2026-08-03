package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/internal/i18n"
)

func TestOfflineLocalDaemonInspectsStoppedService(t *testing.T) {
	inspectCalls := 0
	controller := &ServiceController{
		Inspect: func(context.Context) (ServiceSummary, error) {
			inspectCalls++
			return ServiceSummary{Installed: true, State: "inactive"}, nil
		},
		Start: func(context.Context) error { return nil },
	}
	m := newModelWithOptions(t.Context(), "http://127.0.0.1:19090", RunOptions{ServiceController: controller})
	connectionErr := errors.New("dial tcp 127.0.0.1:19090: connect: connection refused")
	updated, cmd := m.Update(statsMsg{requestID: m.statsRequestID, err: connectionErr})
	m = updated.(Model)
	if cmd == nil || m.serviceState != serviceRecoveryChecking {
		t.Fatalf("inspection was not started: state=%v cmd=%v", m.serviceState, cmd)
	}
	message, ok := cmd().(serviceInspectMsg)
	if !ok {
		t.Fatalf("inspection command returned %T", cmd())
	}
	updated, _ = m.Update(message)
	m = updated.(Model)
	if inspectCalls != 1 || m.serviceState != serviceRecoveryStopped || !m.canStartService() {
		t.Fatalf("inspection result: calls=%d state=%v summary=%+v", inspectCalls, m.serviceState, m.serviceSummary)
	}
}

func TestHTTPAuthorizationFailureDoesNotInspectNativeService(t *testing.T) {
	controller := &ServiceController{Inspect: func(context.Context) (ServiceSummary, error) {
		t.Fatal("native service must not be inspected for an HTTP authorization failure")
		return ServiceSummary{}, nil
	}}
	m := newModelWithOptions(t.Context(), "http://127.0.0.1:19090", RunOptions{ServiceController: controller})
	updated, cmd := m.Update(statsMsg{requestID: m.statsRequestID, err: &APIError{StatusCode: 401, Message: "unauthorized"}})
	m = updated.(Model)
	if cmd != nil || m.serviceState != serviceRecoveryIdle {
		t.Fatalf("authorization failure triggered service recovery: state=%v cmd=%v", m.serviceState, cmd)
	}
}

func TestServiceStartConfirmationAndAutomaticReconnect(t *testing.T) {
	startCalls := 0
	controller := &ServiceController{Start: func(context.Context) error {
		startCalls++
		return nil
	}}
	m := offlineServiceModel(t, controller, i18n.New(i18n.English))

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.overlay != overlayStartService {
		t.Fatalf("start confirmation was not opened: overlay=%v cmd=%v", m.overlay, cmd)
	}
	updated, cmd = m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.serviceState != serviceRecoveryStarting || startCalls != 0 {
		t.Fatalf("service start was not scheduled once: state=%v calls=%d cmd=%v", m.serviceState, startCalls, cmd)
	}

	blocked, duplicate := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = blocked.(Model)
	if duplicate != nil || startCalls != 0 || !strings.Contains(m.statusText, "wait") {
		t.Fatalf("duplicate start was not blocked: calls=%d status=%q cmd=%v", startCalls, m.statusText, duplicate)
	}
	startMessage, ok := cmd().(serviceStartMsg)
	if !ok {
		t.Fatalf("start command returned %T", cmd())
	}
	updated, reconnect := m.Update(startMessage)
	m = updated.(Model)
	if startCalls != 1 || m.serviceState != serviceRecoveryWaiting || reconnect == nil {
		t.Fatalf("start result: calls=%d state=%v reconnect=%v", startCalls, m.serviceState, reconnect)
	}

	updated, _ = m.Update(statsMsg{requestID: m.statsRequestID, resp: &StatsResponse{}})
	m = updated.(Model)
	if m.serviceState != serviceRecoveryIdle || m.connectionErr != nil || !strings.Contains(m.statusText, "connected") {
		t.Fatalf("successful reconnect state=%v err=%v status=%q", m.serviceState, m.connectionErr, m.statusText)
	}
}

func TestServiceStartRequestsElevationAfterPermissionFailure(t *testing.T) {
	controller := &ServiceController{
		Start: func(context.Context) error {
			return fmt.Errorf("%w: start vmflow", ErrServicePrivilegesRequired)
		},
		ElevatedStartCommand: func() (*exec.Cmd, error) {
			return exec.Command("true"), nil
		},
	}
	m := offlineServiceModel(t, controller, i18n.New(i18n.English))
	m.overlay = overlayStartService
	updated, cmd := m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	message := cmd().(serviceStartMsg)
	updated, _ = m.Update(message)
	m = updated.(Model)
	if m.overlay != overlayElevateService || m.serviceState != serviceRecoveryStopped {
		t.Fatalf("permission failure state=%v overlay=%v status=%q", m.serviceState, m.overlay, m.statusText)
	}
	updated, elevated := m.handleOverlayKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if elevated == nil || m.serviceState != serviceRecoveryStarting || m.overlay != overlayNone {
		t.Fatalf("elevated start was not scheduled: state=%v overlay=%v cmd=%v", m.serviceState, m.overlay, elevated)
	}
}

func TestServiceReconnectTimeoutShowsRunningButUnreachable(t *testing.T) {
	m := offlineServiceModel(t, &ServiceController{}, i18n.New(i18n.English))
	m.serviceState = serviceRecoveryWaiting
	m.serviceReconnectDeadline = time.Now().Add(-time.Second)
	m.serviceRequestID = 7
	updated, cmd := m.Update(serviceReconnectMsg{requestID: 7})
	m = updated.(Model)
	if cmd != nil || m.serviceState != serviceRecoveryRunning || !strings.Contains(m.statusText, "unreachable") {
		t.Fatalf("timeout state=%v status=%q cmd=%v", m.serviceState, m.statusText, cmd)
	}
}

func TestStoppedServiceRecoveryFitsCompactAndFullViews(t *testing.T) {
	for _, size := range []struct{ width, height int }{{40, 12}, {80, 24}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := offlineServiceModel(t, &ServiceController{}, i18n.New(i18n.SimplifiedChinese))
			m.width, m.height, m.ready = size.width, size.height, true
			m.viewport.Width = size.width
			m.viewport.Height = max(size.height-3, 1)
			output := m.View()
			assertRenderBounds(t, output, size.width, size.height)
			plain := ansi.Strip(output)
			if !strings.Contains(plain, "启动服务") || !strings.Contains(plain, "已安装") {
				t.Fatalf("stopped-service recovery is not actionable:\n%s", plain)
			}
			if size.width == 40 && strings.Contains(plain, "\n│ vmflow") {
				t.Fatalf("compact recovery stranded the product name on its own line:\n%s", plain)
			}

			m.overlay = overlayStartService
			confirmation := m.View()
			assertRenderBounds(t, confirmation, size.width, size.height)
			confirmationText := ansi.Strip(confirmation)
			if !strings.Contains(confirmationText, "确认") || strings.Contains(confirmationText, "\n│ 。") {
				t.Fatalf("start confirmation is not cleanly bounded:\n%s", confirmationText)
			}
		})
	}
}

func offlineServiceModel(t *testing.T, controller *ServiceController, translator *i18n.Translator) Model {
	t.Helper()
	if controller.Start == nil {
		controller.Start = func(context.Context) error { return nil }
	}
	m := newModelWithOptions(t.Context(), "http://127.0.0.1:19090", RunOptions{
		Translator:        translator,
		ServiceController: controller,
	})
	m.view = viewDashboard
	m.connectionErr = errors.New("dial tcp 127.0.0.1:19090: connect: connection refused")
	m.rulesErr = m.connectionErr
	m.configErr = m.connectionErr
	m.serviceState = serviceRecoveryStopped
	m.serviceSummary = ServiceSummary{Installed: true, State: "inactive"}
	return m
}
