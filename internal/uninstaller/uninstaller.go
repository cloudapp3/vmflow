// Package uninstaller performs a complete one-command uninstall of vmflow:
// it removes the native service, deletes the binary, and purges owned config,
// logs, owned certificate caches, and the self-update cache. External TLS
// certificate and key files referenced by config are deliberately preserved.
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
	"runtime"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/internal/statsstore"
	"github.com/cloudapp3/vmflow/internal/updater"
)

const (
	kindService               = "service"
	kindBinary                = "binary"
	kindFile                  = "file"
	kindDir                   = "dir"
	kindOwnedConfig           = "owned-config"
	kindStatsFile             = "stats-file"
	kindClientProfile         = "client-profile"
	colocatedConfigName       = "config.yaml"
	colocatedConfigMarkerName = ".vmflow-config-owned"
)

// Item describes one artifact the uninstall removes.
type Item struct {
	Path string // filesystem path; empty for the service item
	Kind string // service, binary, file, directory, owned config, or stats file
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

	binPath, warn := selfBinary()
	if warn != "" {
		warnings = append(warnings, warn)
	}

	items, warnings = appendConfigPlan(items, warnings, defaultConfigPath(), kindFile)
	if cfgPath, markerPath, owned := colocatedConfig(binPath); owned {
		if !samePath(cfgPath, defaultConfigPath()) {
			items, warnings = appendConfigPlan(items, warnings, cfgPath, kindOwnedConfig)
		}
		items = append(items, Item{Kind: kindFile, Path: markerPath, Note: "colocated config ownership marker"})
	} else if pathLexists(cfgPath) && !samePath(cfgPath, defaultConfigPath()) {
		warnings = append(warnings, fmt.Sprintf("leaving colocated config %s in place (missing %s ownership marker)", cfgPath, colocatedConfigMarkerName))
	}
	for _, path := range defaultStatsPaths() {
		items, warnings = appendStatsPlan(items, warnings, path)
	}
	for _, path := range defaultStatePaths() {
		if pathExists(path) {
			items = append(items, Item{Kind: kindDir, Path: path, Note: "runtime state directory"})
		}
	}

	for _, p := range defaultLogPaths() {
		if pathExists(p) {
			items = append(items, Item{Kind: kindDir, Path: p, Note: "log directory"})
		}
	}

	if cacheFile := updater.CacheFilePath(); pathExists(cacheFile) {
		if filepath.IsAbs(cacheFile) {
			items = append(items, Item{Kind: kindFile, Path: cacheFile, Note: "self-update cache file"})
		} else {
			warnings = append(warnings, fmt.Sprintf("leaving relative self-update cache path %s in place", cacheFile))
		}
	}
	if profilePath, err := clientconfig.DefaultPath(); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not resolve local management client profile: %v", err))
	} else {
		items, warnings = appendClientProfilePlan(items, warnings, profilePath)
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

func appendClientProfilePlan(items []Item, warnings []string, path string) ([]Item, []string) {
	if !pathLexists(path) || planContainsPath(items, filepath.Clean(path)) {
		return items, warnings
	}
	if _, err := clientconfig.Load(path); err != nil {
		warnings = append(warnings, fmt.Sprintf("leaving local management client profile %s in place: %v", path, err))
		return items, warnings
	}
	items = append(items, Item{Kind: kindClientProfile, Path: filepath.Clean(path), Note: "local management client profile"})
	return items, warnings
}

func appendConfigPlan(items []Item, warnings []string, cfgPath, configKind string) ([]Item, []string) {
	if !pathLexists(cfgPath) {
		return items, warnings
	}
	items = append(items, Item{Kind: configKind, Path: cfgPath, Note: "config file"})
	if !pathExists(cfgPath) {
		return items, warnings
	}
	items, warnings = appendStatsPlan(items, warnings, filepath.Join(filepath.Dir(cfgPath), statsstore.DefaultFilename))
	if statsPath := configuredStatsPath(cfgPath); statsPath != "" {
		items, warnings = appendStatsPlan(items, warnings, statsPath)
	}

	certPaths, cacheDirs := cleanupPathsFromConfig(cfgPath)
	for _, p := range certPaths {
		warnings = append(warnings, fmt.Sprintf("leaving external TLS certificate/key path %s in place (ownership cannot be proven)", p))
	}
	for _, p := range cacheDirs {
		if !filepath.IsAbs(p) {
			warnings = append(warnings, fmt.Sprintf("leaving relative cache path %s in place", p))
			continue
		}
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("leaving cache symlink %s in place (ownership cannot be proven)", p))
			continue
		}
		if !info.IsDir() {
			warnings = append(warnings, fmt.Sprintf("refusing to remove cache path %s because it is not a directory", p))
			continue
		}
		if tlsPath, contains := cacheContainsReferencedTLS(p, certPaths); contains {
			warnings = append(warnings, fmt.Sprintf("leaving cache directory %s in place because it may contain referenced TLS path %s", p, tlsPath))
			continue
		}
		if !canRemoveVMFlowDir(p) {
			warnings = append(warnings, fmt.Sprintf("leaving custom cache directory %s in place (missing %s ownership marker)", p, ownedDirMarker))
			continue
		}
		items = append(items, Item{Kind: kindDir, Path: p, Note: "vmflow-owned certificate cache"})
	}
	return items, warnings
}

func configuredStatsPath(cfgPath string) string {
	cfg, err := config.Load(cfgPath)
	if err != nil || strings.TrimSpace(cfg.Stats.Path) == "" {
		return ""
	}
	return statsstore.ResolvePath(cfgPath, cfg.Stats.Path, "")
}

func appendStatsPlan(items []Item, warnings []string, path string) ([]Item, []string) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !pathLexists(path) || planContainsPath(items, path) {
		return items, warnings
	}
	if err := validateStatsFile(path); err != nil {
		warnings = append(warnings, fmt.Sprintf("leaving stats path %s in place: %v", path, err))
		return items, warnings
	}
	items = append(items, Item{Kind: kindStatsFile, Path: path, Note: "persistent traffic statistics"})
	return items, warnings
}

func planContainsPath(items []Item, path string) bool {
	for _, item := range items {
		if item.Path != "" && samePath(filepath.Clean(item.Path), path) {
			return true
		}
	}
	return false
}

func validateStatsFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular vmflow stats file")
	}
	if _, err := statsstore.New(path).Load(); err != nil {
		return fmt.Errorf("not a valid vmflow stats file: %w", err)
	}
	return nil
}

func colocatedConfig(binaryPath string) (configPath, markerPath string, owned bool) {
	if strings.TrimSpace(binaryPath) == "" {
		return "", "", false
	}
	dir := filepath.Dir(binaryPath)
	configPath = filepath.Join(dir, colocatedConfigName)
	markerPath = filepath.Join(dir, colocatedConfigMarkerName)
	info, err := os.Lstat(markerPath)
	return configPath, markerPath, err == nil && info.Mode().IsRegular()
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
		case kindStatsFile:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if err := validateStatsFile(it.Path); os.IsNotExist(err) {
				continue
			} else if err != nil {
				problems = append(problems, fmt.Sprintf("refusing to remove changed stats file %s: %v", it.Path, err))
				continue
			}
			if err := os.Remove(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintf(w, "removed %s\n", it.Path)
		case kindClientProfile:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if _, err := clientconfig.Load(it.Path); os.IsNotExist(err) {
				continue
			} else if err != nil {
				problems = append(problems, fmt.Sprintf("refusing to remove changed client profile %s: %v", it.Path, err))
				continue
			}
			if err := os.Remove(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintf(w, "removed %s\n", it.Path)
		case kindFile:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if info, err := os.Lstat(it.Path); err == nil && info.IsDir() {
				problems = append(problems, fmt.Sprintf("refusing to remove directory as a file: %s", it.Path))
				continue
			}
			if err := os.Remove(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintf(w, "removed %s\n", it.Path)
		case kindOwnedConfig:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			info, err := os.Lstat(it.Path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				problems = append(problems, fmt.Sprintf("cannot inspect colocated config %s: %v", it.Path, err))
				continue
			}
			if info.IsDir() {
				problems = append(problems, fmt.Sprintf("refusing to remove directory as a config file: %s", it.Path))
				continue
			}
			markerPath := filepath.Join(filepath.Dir(it.Path), colocatedConfigMarkerName)
			markerInfo, err := os.Lstat(markerPath)
			if err != nil || !markerInfo.Mode().IsRegular() {
				problems = append(problems, fmt.Sprintf("refusing to remove colocated config after ownership marker changed: %s", it.Path))
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
			if _, err := os.Lstat(it.Path); err == nil {
				if !canRemoveVMFlowDir(it.Path) {
					problems = append(problems, fmt.Sprintf("refusing to recursively remove directory without vmflow ownership: %s", it.Path))
					continue
				}
			} else if !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("cannot inspect directory before removal %s: %v", it.Path, err))
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

func pathLexists(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

// cleanupPathsFromConfig returns individual certificate files separately from
// cache directories. A config that fails to parse yields no paths; the config
// file itself is still removed by its own Item.
func cleanupPathsFromConfig(cfgPath string) (certFiles, cacheDirs []string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil
	}
	add := func(dst *[]string, p string) {
		if p = strings.TrimSpace(p); p != "" {
			*dst = append(*dst, p)
		}
	}
	add(&certFiles, cfg.ControlTLS.CertFile)
	add(&certFiles, cfg.ControlTLS.KeyFile)
	add(&certFiles, cfg.ControlTLS.ClientCAFile)
	add(&cacheDirs, cfg.AcmeCacheDir)
	add(&cacheDirs, cfg.CertCacheDir)
	return certFiles, cacheDirs
}

const ownedDirMarker = ".vmflow-owned"

func canRemoveVMFlowDir(path string) bool {
	clean := filepath.Clean(path)
	for _, known := range defaultLogPaths() {
		if samePath(clean, filepath.Clean(known)) {
			return true
		}
	}
	for _, known := range defaultStatePaths() {
		if samePath(clean, filepath.Clean(known)) {
			return true
		}
	}
	info, err := os.Lstat(filepath.Join(clean, ownedDirMarker))
	return err == nil && info.Mode().IsRegular()
}

func cacheContainsReferencedTLS(cacheDir string, certPaths []string) (string, bool) {
	resolvedCache := resolvedExistingPath(cacheDir)
	for _, certPath := range certPaths {
		// Runtime-relative paths depend on the daemon's working directory, which
		// the uninstaller cannot reconstruct safely. Preserve the cache rather
		// than risk removing the referenced file indirectly.
		if !filepath.IsAbs(certPath) {
			return certPath, true
		}
		resolvedCert := resolvedExistingPath(certPath)
		rel, err := filepath.Rel(resolvedCache, resolvedCert)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return certPath, true
		}
	}
	return "", false
}

func resolvedExistingPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
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
// or the user's home directory itself).
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
