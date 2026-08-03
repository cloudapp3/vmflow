package tui

import (
	"context"
	"errors"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	serviceReconnectInterval = 500 * time.Millisecond
	serviceReconnectTimeout  = 10 * time.Second
)

type serviceRecoveryState int

const (
	serviceRecoveryDisabled serviceRecoveryState = iota
	serviceRecoveryIdle
	serviceRecoveryChecking
	serviceRecoveryStopped
	serviceRecoveryNotInstalled
	serviceRecoveryRunning
	serviceRecoveryStarting
	serviceRecoveryWaiting
	serviceRecoveryFailed
)

var (
	// ErrServiceNotInstalled classifies a start failure caused by a missing
	// native service definition.
	ErrServiceNotInstalled = errors.New("native service is not installed")
	// ErrServicePrivilegesRequired classifies a start failure that can be
	// retried through ElevatedStartCommand.
	ErrServicePrivilegesRequired = errors.New("administrator privileges are required")
)

// ServiceSummary is the platform-independent native service state needed by
// the TUI recovery flow.
type ServiceSummary struct {
	Installed bool
	Running   bool
	Enabled   bool
	State     string
	Detail    string
}

// ServiceController connects the TUI recovery flow to the native service
// manager. It is supplied only for a known-local management target.
type ServiceController struct {
	Inspect              func(context.Context) (ServiceSummary, error)
	Start                func(context.Context) error
	ElevatedStartCommand func() (*exec.Cmd, error)
	InstallCommand       string
}

type serviceInspectMsg struct {
	requestID int64
	summary   ServiceSummary
	err       error
}

type serviceStartMsg struct {
	requestID int64
	elevated  bool
	err       error
}

type serviceReconnectMsg struct {
	requestID int64
}

func inspectServiceCmd(ctx context.Context, controller *ServiceController, requestID int64) tea.Cmd {
	return func() tea.Msg {
		if controller == nil || controller.Inspect == nil {
			return serviceInspectMsg{requestID: requestID, err: errors.New("native service inspection is unavailable")}
		}
		if err := ctx.Err(); err != nil {
			return serviceInspectMsg{requestID: requestID, err: err}
		}
		summary, err := controller.Inspect(ctx)
		return serviceInspectMsg{requestID: requestID, summary: summary, err: err}
	}
}

func startServiceCmd(ctx context.Context, controller *ServiceController, requestID int64) tea.Cmd {
	return func() tea.Msg {
		if controller == nil || controller.Start == nil {
			return serviceStartMsg{requestID: requestID, err: errors.New("native service start is unavailable")}
		}
		if err := ctx.Err(); err != nil {
			return serviceStartMsg{requestID: requestID, err: err}
		}
		return serviceStartMsg{requestID: requestID, err: controller.Start(ctx)}
	}
}

func serviceReconnectCmd(requestID int64) tea.Cmd {
	return tea.Tick(serviceReconnectInterval, func(time.Time) tea.Msg {
		return serviceReconnectMsg{requestID: requestID}
	})
}

func (m Model) shouldInspectService(err error) bool {
	return m.serviceController != nil && apiStatus(err) == 0 && m.serviceState == serviceRecoveryIdle
}

func (m *Model) beginServiceInspection() tea.Cmd {
	if m.serviceController == nil || m.serviceState == serviceRecoveryStarting || m.serviceState == serviceRecoveryWaiting {
		return nil
	}
	m.serviceRequestID++
	m.serviceState = serviceRecoveryChecking
	m.serviceErr = nil
	return inspectServiceCmd(m.ctx, m.serviceController, m.serviceRequestID)
}

func (m *Model) resetServiceRecovery() {
	if m.serviceController == nil {
		m.serviceState = serviceRecoveryDisabled
	} else {
		m.serviceState = serviceRecoveryIdle
	}
	m.serviceSummary = ServiceSummary{}
	m.serviceErr = nil
	m.serviceReconnectDeadline = time.Time{}
	m.serviceRequestID++
}

func (m Model) canStartService() bool {
	if m.serviceController == nil || m.serviceController.Start == nil || !m.serviceSummary.Installed || m.serviceSummary.Running {
		return false
	}
	return m.serviceState == serviceRecoveryStopped || m.serviceState == serviceRecoveryFailed
}

func (m Model) serviceBusy() bool {
	return m.serviceState == serviceRecoveryStarting || m.serviceState == serviceRecoveryWaiting
}

func (m Model) startService() (tea.Model, tea.Cmd) {
	if !m.canStartService() {
		m.overlay = overlayNone
		m.setStatus(m.t("tui.status.service_start_unavailable"))
		return m, nil
	}
	m.overlay = overlayNone
	m.serviceRequestID++
	m.serviceState = serviceRecoveryStarting
	m.serviceErr = nil
	m.setStatus(m.t("tui.status.service_starting"))
	return m, startServiceCmd(m.ctx, m.serviceController, m.serviceRequestID)
}

func (m Model) startServiceElevated() (tea.Model, tea.Cmd) {
	if m.serviceController == nil || m.serviceController.ElevatedStartCommand == nil {
		m.overlay = overlayNone
		m.serviceState = serviceRecoveryFailed
		m.setStatus(m.t("tui.status.service_elevation_unavailable"))
		return m, nil
	}
	command, err := m.serviceController.ElevatedStartCommand()
	if err != nil {
		m.overlay = overlayNone
		m.serviceState = serviceRecoveryFailed
		m.serviceErr = err
		m.setStatus(m.tf("tui.status.service_start_failed", err))
		return m, nil
	}
	m.overlay = overlayNone
	m.serviceRequestID++
	requestID := m.serviceRequestID
	m.serviceState = serviceRecoveryStarting
	m.serviceErr = nil
	m.setStatus(m.t("tui.status.service_starting_elevated"))
	return m, tea.ExecProcess(command, func(err error) tea.Msg {
		return serviceStartMsg{requestID: requestID, elevated: true, err: err}
	})
}

func (m Model) serviceActivityLabel() string {
	switch m.serviceState {
	case serviceRecoveryChecking:
		return m.spinner.View() + " " + m.t("tui.service.checking")
	case serviceRecoveryStarting:
		return m.spinner.View() + " " + m.t("tui.service.starting")
	case serviceRecoveryWaiting:
		return m.spinner.View() + " " + m.t("tui.service.waiting")
	default:
		return ""
	}
}

func (m Model) serviceRecoveryCopy(compact bool) (string, string) {
	switch m.serviceState {
	case serviceRecoveryChecking:
		if compact {
			return m.t("tui.service.checking_detail_compact"), ""
		}
		return m.t("tui.service.checking_detail"), ""
	case serviceRecoveryStopped:
		if compact {
			return m.t("tui.service.stopped_detail_compact"), m.t("tui.service.start_action")
		}
		return m.t("tui.service.stopped_detail"), m.t("tui.service.start_action")
	case serviceRecoveryNotInstalled:
		if compact {
			return m.t("tui.service.not_installed_detail_compact"), m.t("tui.service.install_outside_tui")
		}
		if m.serviceController == nil || m.serviceController.InstallCommand == "" {
			return m.t("tui.service.not_installed_detail"), m.t("tui.service.install_outside_tui")
		}
		return m.t("tui.service.not_installed_detail"), m.tf("tui.service.install_command", m.serviceController.InstallCommand)
	case serviceRecoveryRunning:
		if compact {
			return m.t("tui.service.running_unreachable_compact"), m.t("tui.service.retry_action")
		}
		return m.t("tui.service.running_unreachable"), m.t("tui.service.retry_action")
	case serviceRecoveryStarting:
		if compact {
			return m.t("tui.service.starting_detail_compact"), ""
		}
		return m.t("tui.service.starting_detail"), ""
	case serviceRecoveryWaiting:
		if compact {
			return m.t("tui.service.waiting_detail_compact"), ""
		}
		return m.t("tui.service.waiting_detail"), ""
	case serviceRecoveryFailed:
		detail := m.t("tui.service.inspect_failed")
		if m.serviceErr != nil {
			detail = m.tf("tui.service.failed_detail", m.serviceErr)
		}
		if m.canStartService() {
			return detail, m.t("tui.service.start_retry_action")
		}
		return detail, m.t("tui.service.retry_action")
	default:
		if compact {
			return "", m.t("tui.dashboard.recovery_compact")
		}
		return "", m.t("tui.dashboard.recovery")
	}
}
