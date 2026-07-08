//go:build darwin

package uninstaller

import (
	"path/filepath"

	"github.com/cloudapp3/vmflow/internal/service"
)

const (
	darwinDefaultCfg = "/usr/local/etc/vmflow/config.yaml"
	darwinLogDir     = "/var/log/vmflow"
	darwinPlistDir   = "/Library/LaunchDaemons"
)

func defaultConfigPath() string { return darwinDefaultCfg }

func defaultLogPaths() []string { return []string{darwinLogDir} }

func serviceInstalled() bool {
	// launchdLabel in internal/service renders "io.cloudapp." + lowercased name.
	label := "io.cloudapp." + service.DefaultServiceName
	return pathExists(filepath.Join(darwinPlistDir, label+".plist"))
}
