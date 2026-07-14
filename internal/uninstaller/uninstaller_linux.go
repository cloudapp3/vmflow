//go:build linux

package uninstaller

import (
	"os/exec"
	"path/filepath"

	"github.com/cloudapp3/vmflow/internal/service"
)

const (
	linuxDefaultCfg = "/etc/vmflow/config.yaml"
	linuxUnitName   = service.DefaultServiceName + ".service"
)

func defaultConfigPath() string { return linuxDefaultCfg }

func defaultLogPaths() []string { return []string{"/var/log/vmflow"} }

func defaultStatePaths() []string { return []string{"/var/lib/vmflow"} }

func defaultStatsPaths() []string { return nil }

func serviceInstalled() bool {
	if err := exec.Command("systemctl", "cat", linuxUnitName).Run(); err == nil {
		return true
	}
	// Fall back to common unit locations for systems without systemctl (or
	// where it cannot reach the unit, e.g. a chroot).
	for _, dir := range []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"} {
		if pathExists(filepath.Join(dir, linuxUnitName)) {
			return true
		}
	}
	return false
}
