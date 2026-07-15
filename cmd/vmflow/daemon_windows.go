//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/service"
	"golang.org/x/sys/windows/svc"
)

// maybeRunAsService detects whether the process was launched by the Windows
// Service Control Manager. If so, it runs the forwarding engine as a native
// service — reporting state to the SCM and handling Stop/Shutdown controls —
// and blocks until the service stops, then returns true. Otherwise it returns
// false and the caller runs the daemon in the foreground as usual.
func maybeRunAsService(cfg, startupConfig config.File, configPath string, logger *slog.Logger, serviceName string) bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		logger.Error("cannot determine if running as a Windows service", "component", "service", "error", err)
		return false
	}
	if !isSvc {
		return false
	}
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		serviceName = service.DefaultServiceName
	}
	if err := svc.Run(serviceName, &vmflowService{cfg: cfg, startupConfig: startupConfig, configPath: configPath, logger: logger}); err != nil {
		logger.Error("service runner failed", "component", "service", "error", err)
		os.Exit(1)
	}
	return true
}

// vmflowService implements svc.Handler, bridging the SCM lifecycle to the
// shared runForwarding engine loop.
type vmflowService struct {
	cfg           config.File
	startupConfig config.File
	configPath    string
	logger        *slog.Logger
}

// Execute is invoked by the SCM. It reports StartPending -> Running, starts the
// forwarding engine in a goroutine, then blocks on SCM controls (Stop/Shutdown)
// or a fatal engine error, after which it performs graceful shutdown and
// reports StopPending. A non-zero service-specific exit code is returned on
// failure so the configured recovery action (restart) fires.
func (m *vmflowService) Execute(_ []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (ssec bool, errno uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readyCh := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runForwardingWithReady(ctx, m.cfg, m.startupConfig, m.configPath, m.logger, readyCh)
	}()

	if err := <-readyCh; err != nil {
		m.logger.Error("service initialization failed", "component", "service", "event", "service_init_failed", "error", err)
		return true, 1
	}
	status <- svc.Status{State: svc.Running, Accepts: accepts}

	var runErr error
	engineDone := false
loop:
	for {
		select {
		case c := <-changes:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				m.logger.Info("service stop requested", "component", "service", "event", "service_stop")
				break loop
			}
		case e := <-errCh:
			runErr = e
			if runErr == nil {
				runErr = fmt.Errorf("forwarding engine stopped unexpectedly")
			}
			engineDone = true
			break loop
		}
	}

	status <- svc.Status{State: svc.StopPending}
	cancel()
	if !engineDone {
		// Give the engine up to a few seconds to finish graceful shutdown.
		select {
		case e := <-errCh:
			if runErr == nil {
				runErr = e
			}
		case <-time.After(6 * time.Second):
			m.logger.Warn("service shutdown timed out", "component", "service", "event", "service_stop_timeout")
		}
	}

	if runErr != nil {
		m.logger.Error("service ended abnormally", "component", "service", "event", "service_failed", "error", runErr)
		// Service-specific non-zero exit → SCM treats it as failed and runs
		// the restart recovery action configured at install time.
		return true, 1
	}
	return false, 0
}
