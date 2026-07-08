//go:build windows

package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/cloudapp3/vmflow/config"
	"golang.org/x/sys/windows/svc"
)

// serviceWinName is the name the daemon registers under with the Windows
// Service Control Manager. It must match the name used by `vmflow service
// install` (the ServiceName, default "vmflow").
const serviceWinName = "vmflow"

// maybeRunAsService detects whether the process was launched by the Windows
// Service Control Manager. If so, it runs the forwarding engine as a native
// service — reporting state to the SCM and handling Stop/Shutdown controls —
// and blocks until the service stops, then returns true. Otherwise it returns
// false and the caller runs the daemon in the foreground as usual.
func maybeRunAsService(cfg config.File, configPath string, logger *slog.Logger, insecure bool) bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		logger.Error("cannot determine if running as a Windows service", "component", "service", "error", err)
		return false
	}
	if !isSvc {
		return false
	}
	if err := svc.Run(serviceWinName, &vmflowService{cfg: cfg, configPath: configPath, logger: logger, insecure: insecure}); err != nil {
		logger.Error("service runner failed", "component", "service", "error", err)
		os.Exit(1)
	}
	return true
}

// vmflowService implements svc.Handler, bridging the SCM lifecycle to the
// shared runForwarding engine loop.
type vmflowService struct {
	cfg        config.File
	configPath string
	logger     *slog.Logger
	insecure   bool
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
	errCh := make(chan error, 1)
	go func() { errCh <- runForwarding(ctx, m.cfg, m.configPath, m.logger, m.insecure) }()

	status <- svc.Status{State: svc.Running, Accepts: accepts}

	var runErr error
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
			break loop
		}
	}

	status <- svc.Status{State: svc.StopPending}
	cancel()
	// Give the engine up to a few seconds to finish graceful shutdown.
	select {
	case e := <-errCh:
		if runErr == nil {
			runErr = e
		}
	case <-time.After(6 * time.Second):
	}

	if runErr != nil {
		m.logger.Error("service ended abnormally", "component", "service", "event", "service_failed", "error", runErr)
		// Service-specific non-zero exit → SCM treats it as failed and runs
		// the restart recovery action configured at install time.
		return true, 1
	}
	return false, 0
}
