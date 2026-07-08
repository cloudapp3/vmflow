// Package uninstaller performs a complete one-command uninstall of vmflow:
// it removes the native service, deletes the binary, and purges config, logs,
// TLS/ACME certificates, and the self-update cache.
//
// The flow is plan → confirm → execute. Plan probes the system and returns the
// ordered list of items to remove (service first, the running binary last).
// Confirm asks for interactive consent. Execute removes everything, tolerating
// already-absent paths so the command is idempotent.
//
// Style follows internal/service and internal/updater: error-return, progress
// streamed to an io.Writer, no logging inside the package.
package uninstaller

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/internal/updater"
)

const (
	kindService = "service"
	kindBinary  = "binary"
	kindFile    = "file"
	kindDir     = "dir"
)

// Item describes one artifact the uninstall removes.
type Item struct {
	Path string // filesystem path; empty for the service item
	Kind string // kindService | kindBinary | kindFile | kindDir
	Note string // human-readable description shown in the plan
	Self bool   // marks the running binary; removed last
}

// Plan probes the system and returns the ordered list of items that would be
// removed, plus warnings. The service is removed first and the running binary
// last so a still-supervised daemon is gone before its executable is deleted.
func Plan() (items []Item, warnings []string) {
	if serviceInstalled() {
		items = append(items, Item{
			Kind: kindService,
			Note: "stop and remove native service (systemd / launchd / Windows Service)",
		})
	}

	if cfgPath := defaultConfigPath(); pathExists(cfgPath) {
		items = append(items, Item{Kind: kindFile, Path: cfgPath, Note: "config file"})
		for _, p := range certPathsFromConfig(cfgPath) {
			if pathExists(p) {
				items = append(items, Item{Kind: certKind(p), Path: p, Note: "TLS / ACME certificate (referenced by config)"})
			}
		}
	}

	for _, p := range defaultLogPaths() {
		if pathExists(p) {
			items = append(items, Item{Kind: kindDir, Path: p, Note: "log directory"})
		}
	}

	if cache := updater.CacheDir(); pathExists(cache) {
		items = append(items, Item{Kind: kindDir, Path: cache, Note: "self-update cache"})
	}

	binPath, warn := selfBinary()
	if warn != "" {
		warnings = append(warnings, warn)
	}
	if binPath != "" {
		if owner := packageOwner(binPath); owner != "" {
			warnings = append(warnings,
				fmt.Sprintf("binary %s is owned by a package manager (%s); a cleaner removal is `apt remove`/`yum remove` — continuing will leave the dpkg/rpm database stale", binPath, owner))
		}
		items = append(items, Item{Kind: kindBinary, Path: binPath, Note: "vmflow binary", Self: true})
	}
	return items, warnings
}

// Print writes the plan (warnings + items) to w.
func Print(w io.Writer, items []Item, warnings []string) {
	if len(warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, wn := range warnings {
			fmt.Fprintf(w, "  ! %s\n", wn)
		}
		fmt.Fprintln(w)
	}
	if len(items) == 0 {
		fmt.Fprintln(w, "Nothing to remove: vmflow does not appear to be installed.")
		return
	}
	fmt.Fprintln(w, "The following will be removed:")
	for i, it := range items {
		switch {
		case it.Kind == kindService:
			fmt.Fprintf(w, "  %2d. [service] %s\n", i+1, it.Note)
		case it.Self:
			fmt.Fprintf(w, "  %2d. [%s] %s  (running binary; removed last) — %s\n", i+1, it.Kind, it.Path, it.Note)
		default:
			fmt.Fprintf(w, "  %2d. [%s] %s — %s\n", i+1, it.Kind, it.Path, it.Note)
		}
	}
	fmt.Fprintln(w)
}

// Confirm prompts on w and reads a yes/no answer from r. Returns true only for
// an explicit y/yes.
func Confirm(w io.Writer, r io.Reader) (bool, error) {
	fmt.Fprint(w, "Proceed with uninstall? [y/N] ")
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return ans == "y" || ans == "yes", nil
}

// Execute removes every item in order. Already-absent paths are not errors, so
// re-running the command is safe. The running binary is removed last.
func Execute(w io.Writer, items []Item) error {
	var problems []string
	for _, it := range items {
		switch it.Kind {
		case kindService:
			if err := service.Uninstall(service.Config{ServiceName: service.DefaultServiceName}, w); err != nil {
				problems = append(problems, fmt.Sprintf("service: %v", err))
			}
		case kindFile:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if err := os.Remove(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintf(w, "removed %s\n", it.Path)
		case kindDir:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if err := os.RemoveAll(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintf(w, "removed %s\n", it.Path)
		case kindBinary:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if err := removeSelf(it.Path, w); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
			}
		}
	}
	if len(problems) > 0 {
		fmt.Fprintln(w, "uninstall completed with errors:")
		for _, p := range problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
		return fmt.Errorf("uninstall completed with %d error(s)", len(problems))
	}
	fmt.Fprintln(w, "uninstall complete")
	return nil
}

// selfBinary returns the absolute path of the running vmflow executable and a
// warning if it could not be determined.
func selfBinary() (string, string) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Sprintf("could not determine vmflow binary path: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, ""
	}
	return resolved, ""
}

// pathExists reports whether path exists on disk.
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// certKind returns kindDir when p is a directory, else kindFile.
func certKind(p string) string {
	if info, err := os.Lstat(p); err == nil && info.IsDir() {
		return kindDir
	}
	return kindFile
}

// certPathsFromConfig loads the config at cfgPath and returns the certificate /
// key / cache paths it references. A config that fails to parse yields nothing
// (the config file itself is still removed by its own Item).
func certPathsFromConfig(cfgPath string) []string {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil
	}
	var paths []string
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	add(cfg.ControlTLS.CertFile)
	add(cfg.ControlTLS.KeyFile)
	add(cfg.ControlTLS.ClientCAFile)
	add(cfg.AcmeCacheDir)
	add(cfg.CertCacheDir)
	return paths
}

// packageOwner returns a "tool: owner" label when the binary is managed by
// dpkg or rpm, otherwise "". Commands that do not exist (e.g. on macOS/Windows)
// simply resolve to "".
func packageOwner(path string) string {
	for _, argv := range [][]string{
		{"dpkg-query", "-S", path},
		{"rpm", "-qf", path},
	} {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(out))
		if s == "" {
			continue
		}
		// dpkg-query prints "pkg: path"; rpm prints the package name.
		owner := s
		if pkg, _, ok := strings.Cut(s, ":"); ok {
			owner = strings.TrimSpace(pkg)
		}
		return argv[0] + ": " + owner
	}
	return ""
}

// isProtected reports whether removing path would be dangerous (a system root
// or the user's home directory itself). Config-referenced certificate paths are
// validated through this before deletion.
func isProtected(path string) bool {
	clean := filepath.Clean(path)
	if isProtectedPath(clean) {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && clean == filepath.Clean(home) {
		return true
	}
	return false
}
