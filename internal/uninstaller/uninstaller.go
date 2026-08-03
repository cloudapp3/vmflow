// Package uninstaller performs a complete one-command uninstall of vmflow:
// it removes the native service, deletes the binary, and purges owned config,
// logs, owned certificate caches, and the self-update cache. External TLS
// certificate and key files referenced by config are deliberately preserved.
//
// The flow is plan → confirm → execute. Plan probes the system and returns the
// ordered list of items to remove (service first, the running binary last).
// Confirm asks for interactive consent. Execute requires service cleanup to
// succeed before removing files and tolerates already-absent paths so retries
// remain safe.
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
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/internal/statsstore"
	"github.com/cloudapp3/vmflow/internal/updater"
)

const (
	kindService       = "service"
	kindBinary        = "binary"
	kindFile          = "file"
	kindDir           = "dir"
	kindManagedConfig = "managed-config-dir"
	kindStatsFile     = "stats-file"
	kindQuotaFile     = "quota-file"
	kindClientProfile = "client-profile"
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

	for _, scoped := range []struct {
		scope appdirs.Scope
		name  string
	}{
		{scope: appdirs.System, name: "system"},
		{scope: appdirs.User, name: "user"},
	} {
		cfgPath, err := appdirs.ConfigPath(scoped.scope)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not resolve %s config path: %v", scoped.name, err))
			continue
		}
		items, warnings = appendManagedConfigPlan(items, warnings, cfgPath)
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
	if !pathLexists(path) || planRemovesPath(items, filepath.Clean(path)) {
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
	items, warnings = appendQuotaPlan(items, warnings, filepath.Join(filepath.Dir(cfgPath), statsstore.DefaultQuotaFilename))
	if statsPath := configuredStatsPath(cfgPath); statsPath != "" {
		items, warnings = appendStatsPlan(items, warnings, statsPath)
		items, warnings = appendQuotaPlan(items, warnings, filepath.Join(filepath.Dir(statsPath), statsstore.DefaultQuotaFilename))
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

func appendManagedConfigPlan(items []Item, warnings []string, cfgPath string) ([]Item, []string) {
	dir := filepath.Dir(cfgPath)
	if !pathLexists(dir) {
		return items, warnings
	}
	if !managedConfigMarkerValid(dir) {
		if pathLexists(cfgPath) {
			warnings = append(warnings, fmt.Sprintf("leaving config %s in place (missing valid %s ownership marker)", cfgPath, appdirs.ManagedMarkerName))
		}
		return items, warnings
	}
	if tlsPath, contains := managedConfigContainsReferencedTLS(dir, cfgPath); contains {
		warnings = append(warnings, fmt.Sprintf("leaving managed config directory %s in place because it contains referenced external TLS path %s", dir, tlsPath))
		return items, warnings
	}
	items, warnings = appendConfigPlan(items, warnings, cfgPath, kindFile)
	items = removePlannedPathsWithin(items, dir)
	if !planContainsPath(items, filepath.Clean(dir)) {
		items = append(items, Item{Kind: kindManagedConfig, Path: filepath.Clean(dir), Note: "managed config directory"})
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
	if path == "." || !pathLexists(path) || planRemovesPath(items, path) {
		return items, warnings
	}
	if err := validateStatsFile(path); err != nil {
		warnings = append(warnings, fmt.Sprintf("leaving stats path %s in place: %v", path, err))
		return items, warnings
	}
	items = append(items, Item{Kind: kindStatsFile, Path: path, Note: "persistent traffic statistics"})
	return items, warnings
}

func appendQuotaPlan(items []Item, warnings []string, path string) ([]Item, []string) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !pathLexists(path) || planRemovesPath(items, path) {
		return items, warnings
	}
	if err := validateQuotaFile(path); err != nil {
		warnings = append(warnings, fmt.Sprintf("leaving quota state path %s in place: %v", path, err))
		return items, warnings
	}
	items = append(items, Item{Kind: kindQuotaFile, Path: path, Note: "persistent traffic quota state"})
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

func planRemovesPath(items []Item, path string) bool {
	path = filepath.Clean(path)
	for _, item := range items {
		if item.Path == "" {
			continue
		}
		itemPath := filepath.Clean(item.Path)
		if samePath(itemPath, path) {
			return true
		}
		if (item.Kind == kindDir || item.Kind == kindManagedConfig) && pathWithinDirectory(itemPath, path) {
			return true
		}
	}
	return false
}

func removePlannedPathsWithin(items []Item, dir string) []Item {
	dir = filepath.Clean(dir)
	filtered := items[:0]
	for _, item := range items {
		if item.Path != "" && pathWithinDirectory(dir, filepath.Clean(item.Path)) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func pathWithinDirectory(dir, candidate string) bool {
	if samePath(dir, candidate) {
		return true
	}
	relative, err := filepath.Rel(dir, candidate)
	if err != nil || relative == "." || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
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

func validateQuotaFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular vmflow quota state file")
	}
	if _, err := statsstore.NewQuotaStore(path).LoadQuotaState(); err != nil {
		return fmt.Errorf("not a valid vmflow quota state file: %w", err)
	}
	return nil
}

// Print writes the plan (warnings + items) to w.
func Print(w io.Writer, items []Item, warnings []string) {
	PrintWithTranslator(w, items, warnings, i18n.New(i18n.English))
}

// PrintWithTranslator writes the localized plan while preserving paths and
// diagnostic warning text exactly as discovered by Plan.
func PrintWithTranslator(w io.Writer, items []Item, warnings []string, translator *i18n.Translator) {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	if len(warnings) > 0 {
		fmt.Fprintln(w, translator.T("cli.uninstall.plan.warnings"))
		for _, wn := range warnings {
			fmt.Fprintf(w, "  ! %s\n", wn)
		}
		fmt.Fprintln(w)
	}
	if len(items) == 0 {
		fmt.Fprintln(w, translator.T("cli.uninstall.plan.nothing"))
		return
	}
	fmt.Fprintln(w, translator.T("cli.uninstall.plan.following"))
	for i, it := range items {
		note := localizedItemNote(translator, it.Note)
		switch {
		case it.Kind == kindService:
			fmt.Fprintln(w, translator.Tf("cli.uninstall.plan.item_service", i+1, note))
		case it.Self:
			fmt.Fprintln(w, translator.Tf("cli.uninstall.plan.item_self", i+1, it.Kind, it.Path, note))
		default:
			fmt.Fprintln(w, translator.Tf("cli.uninstall.plan.item", i+1, it.Kind, it.Path, note))
		}
	}
	fmt.Fprintln(w)
}

func localizedItemNote(translator *i18n.Translator, note string) string {
	keys := map[string]string{
		"stop and remove native service (systemd / launchd / Windows Service)": "cli.uninstall.note.service",
		"managed config directory":        "cli.uninstall.note.config_dir",
		"runtime state directory":         "cli.uninstall.note.state_dir",
		"log directory":                   "cli.uninstall.note.log_dir",
		"self-update cache file":          "cli.uninstall.note.update_cache",
		"local management client profile": "cli.uninstall.note.client_profile",
		"config file":                     "cli.uninstall.note.config",
		"vmflow-owned certificate cache":  "cli.uninstall.note.cert_cache",
		"persistent traffic statistics":   "cli.uninstall.note.stats",
		"persistent traffic quota state":  "cli.uninstall.note.stats",
		"vmflow binary":                   "cli.uninstall.note.binary",
	}
	if key := keys[note]; key != "" {
		return translator.T(key)
	}
	return note
}

// Confirm prompts on w and reads a yes/no answer from r. Returns true only for
// an explicit y/yes.
func Confirm(w io.Writer, r io.Reader) (bool, error) {
	return ConfirmWithTranslator(w, r, i18n.New(i18n.English))
}

// ConfirmWithTranslator prompts for explicit consent in the selected language.
func ConfirmWithTranslator(w io.Writer, r io.Reader, translator *i18n.Translator) (bool, error) {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	fmt.Fprint(w, translator.T("cli.uninstall.confirm"))
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return ans == "y" || ans == "yes" || ans == "是", nil
}

// Execute handles native service items first, then removes filesystem items in
// order. Already-absent paths are not errors, so re-running the command is safe.
// The running binary is removed last in plans produced by Plan.
func Execute(w io.Writer, items []Item) error {
	return ExecuteWithTranslator(w, items, i18n.New(i18n.English))
}

// ExecuteWithTranslator localizes progress headings without changing
// validation, paths, or returned errors.
func ExecuteWithTranslator(w io.Writer, items []Item, translator *i18n.Translator) error {
	return executeWithServiceUninstaller(w, items, translator, service.Uninstall)
}

type serviceUninstaller func(service.Config, io.Writer) error

func executeWithServiceUninstaller(w io.Writer, items []Item, translator *i18n.Translator, uninstallService serviceUninstaller) error {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	// Stop and remove the native service before touching any filesystem
	// artifact, even if a caller supplies items in an unexpected order. A
	// failed stop must leave the binary and configuration available for retry.
	for _, it := range items {
		if it.Kind != kindService {
			continue
		}
		if err := uninstallService(service.Config{ServiceName: service.DefaultServiceName, Translator: translator}, w); err != nil {
			fmt.Fprintln(w, translator.T("cli.uninstall.output.service_stop_failed"))
			fmt.Fprintf(w, "  - service: %v\n", err)
			return fmt.Errorf("uninstall stopped before file removal: service: %w", err)
		}
	}

	var problems []string
	for _, it := range items {
		switch it.Kind {
		case kindService:
			continue
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
			fmt.Fprintln(w, translator.Tf("cli.uninstall.output.removed", it.Path))
		case kindQuotaFile:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if err := validateQuotaFile(it.Path); os.IsNotExist(err) {
				continue
			} else if err != nil {
				problems = append(problems, fmt.Sprintf("refusing to remove changed quota state file %s: %v", it.Path, err))
				continue
			}
			if err := os.Remove(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintln(w, translator.Tf("cli.uninstall.output.removed", it.Path))
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
			fmt.Fprintln(w, translator.Tf("cli.uninstall.output.removed", it.Path))
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
			fmt.Fprintln(w, translator.Tf("cli.uninstall.output.removed", it.Path))
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
			fmt.Fprintln(w, translator.Tf("cli.uninstall.output.removed", it.Path))
		case kindManagedConfig:
			if isProtected(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove protected path: %s", it.Path))
				continue
			}
			if !canonicalConfigDir(filepath.Clean(it.Path)) || !managedConfigMarkerValid(it.Path) {
				problems = append(problems, fmt.Sprintf("refusing to remove config directory after ownership marker changed: %s", it.Path))
				continue
			}
			configPath := filepath.Join(it.Path, appdirs.ConfigFilename)
			if tlsPath, contains := managedConfigContainsReferencedTLS(it.Path, configPath); contains {
				problems = append(problems, fmt.Sprintf("refusing to remove config directory %s because it contains referenced external TLS path %s", it.Path, tlsPath))
				continue
			}
			if err := os.RemoveAll(it.Path); err != nil && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			fmt.Fprintln(w, translator.Tf("cli.uninstall.output.removed", it.Path))
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
		fmt.Fprintln(w, translator.T("cli.uninstall.output.errors"))
		for _, p := range problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
		return fmt.Errorf("uninstall completed with %d error(s)", len(problems))
	}
	fmt.Fprintln(w, translator.T("cli.uninstall.output.complete"))
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
	if canonicalConfigDir(clean) && managedConfigMarkerValid(clean) {
		return true
	}
	info, err := os.Lstat(filepath.Join(clean, ownedDirMarker))
	return err == nil && info.Mode().IsRegular()
}

func canonicalConfigDir(candidate string) bool {
	for _, scope := range []appdirs.Scope{appdirs.System, appdirs.User} {
		configPath, err := appdirs.ConfigPath(scope)
		if err == nil && samePath(candidate, filepath.Dir(configPath)) {
			return true
		}
	}
	return false
}

func managedConfigMarkerValid(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	markerPath := filepath.Join(dir, appdirs.ManagedMarkerName)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil || markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return false
	}
	raw, err := os.ReadFile(markerPath)
	return err == nil && strings.TrimSpace(string(raw)) == "vmflow"
}

func managedConfigContainsReferencedTLS(dir, configPath string) (string, bool) {
	certPaths, _ := cleanupPathsFromConfig(configPath)
	for _, certPath := range certPaths {
		candidate := filepath.Clean(certPath)
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(dir, candidate)
		}
		if pathLexists(candidate) && pathWithinDirectory(filepath.Clean(dir), filepath.Clean(candidate)) {
			return certPath, true
		}
	}
	return "", false
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
