//go:build windows

package uninstaller

import (
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/cloudapp3/vmflow/internal/service"
)

const windowsDefaultCfg = `C:\ProgramData\vmflow\config.yaml`

func defaultConfigPath() string { return windowsDefaultCfg }

func defaultLogPaths() []string { return []string{`C:\ProgramData\vmflow\logs`} }

func defaultStatePaths() []string { return nil }

func defaultStatsPaths() []string { return []string{`C:\ProgramData\vmflow\stats.json`} }

func serviceInstalled() bool {
	return exec.Command("sc.exe", "query", service.DefaultServiceName).Run() == nil
}

// windowsProtected are roots whose removal would be catastrophic.
var windowsProtected = []string{
	`C:\`, `C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`,
	`C:\ProgramData`, `C:\Users`,
}

func isProtectedPath(clean string) bool {
	return slices.ContainsFunc(windowsProtected, func(p string) bool {
		return strings.EqualFold(clean, p)
	})
}

// removeSelf cannot delete the running .exe directly on Windows. It spawns a
// detached cmd that waits ~2s (long enough for this process to exit) and then
// deletes the binary.
func removeSelf(path string, w io.Writer) error {
	cmd := exec.Command("cmd", "/c", `ping -n 3 127.0.0.1 >nul & del /q "`+path+`"`)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("schedule binary deletion: %w", err)
	}
	// Detach: do not wait, or this process would hold the file open.
	fmt.Fprintf(w, "scheduled deletion of %s (removed once this process exits)\n", path)
	return nil
}
