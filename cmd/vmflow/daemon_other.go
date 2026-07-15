//go:build !windows

package main

import (
	"log/slog"

	"github.com/cloudapp3/vmflow/config"
)

// maybeRunAsService is a no-op on non-Windows platforms: the daemon always runs
// in the foreground and is supervised by the OS service manager (systemd /
// launchd) directly via signals. Returns false so the caller proceeds to
// runForwarding.
func maybeRunAsService(_, _ config.File, _ string, _ *slog.Logger, _ string) bool {
	return false
}
