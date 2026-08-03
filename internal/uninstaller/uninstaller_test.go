//go:build linux || darwin

package uninstaller

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/internal/statsstore"
)

func TestCleanupPathsFromConfig(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "control.crt")
	key := filepath.Join(dir, "control.key")
	ca := filepath.Join(dir, "clients-ca.crt")
	acme := filepath.Join(dir, "acme")
	cc := filepath.Join(dir, "certcache")

	cfg := strings.Join([]string{
		"version: 1",
		"control_tls:",
		"  cert_file: " + cert,
		"  key_file: " + key,
		"  client_ca_file: " + ca,
		"acme_cache_dir: " + acme,
		"cert_cache_dir: " + cc,
		"",
	}, "\n")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	certFiles, cacheDirs := cleanupPathsFromConfig(cfgPath)
	if len(certFiles) != 3 {
		t.Fatalf("got %d certificate paths %v, want 3", len(certFiles), certFiles)
	}
	want := map[string]bool{cert: true, key: true, ca: true}
	for _, p := range certFiles {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
	if len(cacheDirs) != 2 || cacheDirs[0] != acme || cacheDirs[1] != cc {
		t.Fatalf("unexpected cache dirs: %v", cacheDirs)
	}
}

func TestCleanupPathsFromConfigUnparseable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// config.Load rejects unsupported versions, so cleanupPathsFromConfig yields nil.
	if err := os.WriteFile(cfgPath, []byte("version: 999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	certFiles, cacheDirs := cleanupPathsFromConfig(cfgPath)
	if certFiles != nil || cacheDirs != nil {
		t.Fatalf("expected no paths for unparseable config, got %v %v", certFiles, cacheDirs)
	}
}

func TestAppendManagedConfigPlanRequiresOwnershipMarker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	configPath, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, appdirs.ManagedMarkerName)
	if err := os.WriteFile(configPath, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendManagedConfigPlan(nil, nil, configPath)
	if len(items) != 0 || !strings.Contains(strings.Join(warnings, "\n"), appdirs.ManagedMarkerName) {
		t.Fatalf("unmarked config plan = %+v, warnings = %v", items, warnings)
	}
	if err := os.WriteFile(markerPath, []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, warnings = appendManagedConfigPlan(nil, nil, configPath)
	if len(warnings) != 0 || len(items) != 1 || items[0].Kind != kindManagedConfig || items[0].Path != dir {
		t.Fatalf("managed config plan = %+v, warnings = %v", items, warnings)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(configPath, markerPath); err != nil {
		t.Fatal(err)
	}
	items, _ = appendManagedConfigPlan(nil, nil, configPath)
	if len(items) != 0 {
		t.Fatalf("symlink marker was trusted: %+v", items)
	}
}

func TestManagedConfigDirectoryPlanCoversContainedStatsAndClientProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	configPath, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, appdirs.ManagedMarkerName), []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	statsPath := filepath.Join(dir, statsstore.DefaultFilename)
	if err := statsstore.New(statsPath).Save(nil); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(dir, "client.yaml")
	if err := clientconfig.Save(profilePath, clientconfig.Profile{Address: "http://127.0.0.1:19090", Token: "secret-token", ConfigPath: configPath}); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendManagedConfigPlan(nil, nil, configPath)
	items, warnings = appendStatsPlan(items, warnings, statsPath)
	items, warnings = appendClientProfilePlan(items, warnings, profilePath)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(items) != 1 || items[0].Kind != kindManagedConfig || items[0].Path != dir {
		t.Fatalf("contained paths produced duplicate plan items: %+v", items)
	}
}

func TestManagedConfigDirectoryPreservesReferencedTLSInsideDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	configPath, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "control.crt")
	keyPath := filepath.Join(dir, "control.key")
	for _, path := range []string{certPath, keyPath} {
		if err := os.WriteFile(path, []byte("external TLS material\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configBody := "version: 1\ncontrol_tls:\n  cert_file: " + certPath + "\n  key_file: " + keyPath + "\nrules: []\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, appdirs.ManagedMarkerName), []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendManagedConfigPlan(nil, nil, configPath)
	if len(items) != 0 || !strings.Contains(strings.Join(warnings, "\n"), certPath) {
		t.Fatalf("referenced TLS plan = %+v, warnings = %v", items, warnings)
	}
	if err := Execute(io.Discard, []Item{{Kind: kindManagedConfig, Path: dir}}); err == nil {
		t.Fatal("execution did not recheck referenced TLS")
	}
	for _, path := range []string{configPath, certPath, keyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved path %s changed: %v", path, err)
		}
	}
}

func TestPlanRemovesPathDoesNotTreatSiblingPrefixAsChild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vmflow")
	items := []Item{{Kind: kindManagedConfig, Path: dir}}
	if !planRemovesPath(items, filepath.Join(dir, "client.yaml")) {
		t.Fatal("managed directory did not cover a child path")
	}
	if planRemovesPath(items, dir+"-backup/config.yaml") {
		t.Fatal("sibling path sharing a prefix was treated as a child")
	}
}

func TestPlanHelpersKeepRelativePathsSeparate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\ncontrol_tls:\n  cert_file: relative.crt\n  key_file: relative.key\nacme_cache_dir: relative-cache\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	certFiles, cacheDirs := cleanupPathsFromConfig(cfgPath)
	if len(certFiles) != 2 || filepath.IsAbs(certFiles[0]) || filepath.IsAbs(certFiles[1]) {
		t.Fatalf("expected relative certificate paths to remain identifiable: %v", certFiles)
	}
	if len(cacheDirs) != 1 || filepath.IsAbs(cacheDirs[0]) {
		t.Fatalf("expected relative cache path to remain identifiable: %v", cacheDirs)
	}
}

func TestAppendConfigPlanPreservesExternalTLSFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "shared.crt")
	keyPath := filepath.Join(dir, "shared.key")
	for _, path := range []string{certPath, keyPath} {
		if err := os.WriteFile(path, []byte("shared\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "version: 1\ncontrol_tls:\n  cert_file: " + certPath + "\n  key_file: " + keyPath + "\nrules: []\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendConfigPlan(nil, nil, cfgPath, kindFile)
	if len(items) != 1 || items[0].Path != cfgPath {
		t.Fatalf("external TLS files unexpectedly entered removal plan: %+v", items)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, certPath) || !strings.Contains(joined, keyPath) {
		t.Fatalf("preservation warnings do not name both external TLS files: %v", warnings)
	}
}

func TestAppendConfigPlanIncludesConfiguredStatsFile(t *testing.T) {
	dir := t.TempDir()
	statsPath := filepath.Join(dir, "traffic.json")
	if err := statsstore.New(statsPath).Save(nil); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nstats:\n  persist: true\n  path: traffic.json\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendConfigPlan(nil, nil, configPath, kindFile)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	found := false
	for _, item := range items {
		if item.Kind == kindStatsFile && item.Path == statsPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("configured stats file missing from plan: %+v", items)
	}
}

func TestAppendClientProfilePlanValidatesBeforeRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	if err := clientconfig.Save(path, clientconfig.Profile{Address: "http://127.0.0.1:19090", Token: "secret-token"}); err != nil {
		t.Fatal(err)
	}
	items, warnings := appendClientProfilePlan(nil, nil, path)
	if len(warnings) != 0 || len(items) != 1 || items[0].Kind != kindClientProfile {
		t.Fatalf("client profile plan = %+v, warnings = %v", items, warnings)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(io.Discard, items); err == nil {
		t.Fatal("changed client profile should not be removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("changed client profile was removed: %v", err)
	}
}

func TestExecuteStatsFileRevalidatesBeforeRemoval(t *testing.T) {
	dir := t.TempDir()
	statsPath := filepath.Join(dir, "stats.json")
	if err := statsstore.New(statsPath).Save(nil); err != nil {
		t.Fatal(err)
	}
	if err := Execute(io.Discard, []Item{{Kind: kindStatsFile, Path: statsPath}}); err != nil {
		t.Fatalf("remove valid stats file: %v", err)
	}
	if pathLexists(statsPath) {
		t.Fatal("valid stats file was not removed")
	}

	if err := os.WriteFile(statsPath, []byte("not vmflow stats"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(io.Discard, []Item{{Kind: kindStatsFile, Path: statsPath}}); err == nil {
		t.Fatal("changed stats file should not be removed")
	}
	if !pathLexists(statsPath) {
		t.Fatal("changed stats file was removed")
	}
}

func TestExecuteQuotaFileRevalidatesBeforeRemoval(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, statsstore.DefaultQuotaFilename)
	store := statsstore.NewQuotaStore(quotaPath)
	if err := store.SaveQuotaState([]engine.QuotaPersistentState{{RuleID: "limited", Daily: map[string]int64{"2026-08-02": 10}}}); err != nil {
		t.Fatal(err)
	}
	if err := Execute(io.Discard, []Item{{Kind: kindQuotaFile, Path: quotaPath}}); err != nil {
		t.Fatalf("remove valid quota state: %v", err)
	}
	if pathLexists(quotaPath) {
		t.Fatal("valid quota state was not removed")
	}

	if err := os.WriteFile(quotaPath, []byte("not vmflow quota state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(io.Discard, []Item{{Kind: kindQuotaFile, Path: quotaPath}}); err == nil {
		t.Fatal("changed quota state should not be removed")
	}
	if !pathLexists(quotaPath) {
		t.Fatal("changed quota state was removed")
	}
}

func TestAppendConfigPlanPreservesCacheContainingReferencedTLS(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cert-cache")
	if err := os.Mkdir(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, ownedDirMarker), []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(cacheDir, "control.crt")
	if err := os.WriteFile(certPath, []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(cacheDir, "control.key")
	if err := os.WriteFile(keyPath, []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "version: 1\ncontrol_tls:\n  cert_file: " + certPath + "\n  key_file: " + keyPath + "\ncert_cache_dir: " + cacheDir + "\nrules: []\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendConfigPlan(nil, nil, cfgPath, kindFile)
	for _, item := range items {
		if item.Path == cacheDir {
			t.Fatalf("cache containing referenced TLS file entered removal plan: %+v", items)
		}
	}
	if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, certPath) || !strings.Contains(joined, cacheDir) {
		t.Fatalf("cache-preservation warning is incomplete: %v", warnings)
	}
}

func TestAppendConfigPlanPreservesUnownedCacheSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cache-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheLink := filepath.Join(dir, "cache-link")
	if err := os.Symlink(target, cacheLink); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "version: 1\ncert_cache_dir: " + cacheLink + "\nrules: []\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	items, warnings := appendConfigPlan(nil, nil, cfgPath, kindFile)
	for _, item := range items {
		if item.Path == cacheLink {
			t.Fatalf("unowned cache symlink entered removal plan: %+v", items)
		}
	}
	if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, cacheLink) {
		t.Fatalf("cache symlink preservation warning is missing: %v", warnings)
	}
}

func TestCanRemoveVMFlowDirRequiresMarker(t *testing.T) {
	dir := t.TempDir()
	if canRemoveVMFlowDir(dir) {
		t.Fatal("unmarked custom directory should not be removable")
	}
	if err := os.WriteFile(filepath.Join(dir, ownedDirMarker), []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !canRemoveVMFlowDir(dir) {
		t.Fatal("marked vmflow directory should be removable")
	}
}

func TestIsProtected(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root", "/", true},
		{"etc", "/etc", true},
		{"var", "/var", true},
		{"usr", "/usr", true},
		{"etc_vmflow", "/etc/vmflow", false},
		{"var_log_vmflow", "/var/log/vmflow", false},
		{"binary", "/usr/local/bin/vmflow", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isProtected(c.path); got != c.want {
				t.Errorf("isProtected(%q)=%v want %v", c.path, got, c.want)
			}
		})
	}
}

func TestExecuteRemovesFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ownedDirMarker), []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested", "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []Item{
		{Kind: kindFile, Path: file},
		{Kind: kindDir, Path: sub},
	}
	if err := Execute(io.Discard, items); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pathExists(file) {
		t.Errorf("%s should have been removed", file)
	}
	if pathExists(sub) {
		t.Errorf("%s should have been removed", sub)
	}
}

func TestExecuteStopsBeforeRemovingFilesWhenServiceUninstallFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	binaryPath := filepath.Join(dir, "vmflow")
	for _, path := range []string{configPath, binaryPath} {
		if err := os.WriteFile(path, []byte("keep\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	items := []Item{
		{Kind: kindFile, Path: configPath},
		{Kind: kindService},
		{Kind: kindBinary, Path: binaryPath},
	}
	serviceCalls := 0
	var output bytes.Buffer
	err := executeWithServiceUninstaller(&output, items, i18n.New(i18n.English), func(cfg service.Config, w io.Writer) error {
		serviceCalls++
		if cfg.ServiceName != service.DefaultServiceName {
			t.Fatalf("service name = %q", cfg.ServiceName)
		}
		return errors.New("stop failed")
	})
	if err == nil || !strings.Contains(err.Error(), "stopped before file removal") {
		t.Fatalf("service uninstall error = %v", err)
	}
	if serviceCalls != 1 {
		t.Fatalf("service uninstall calls = %d, want 1", serviceCalls)
	}
	for _, path := range []string{configPath, binaryPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s changed after service stop failure: %v", path, err)
		}
	}
	if got := output.String(); !strings.Contains(got, "native service may still be running") || strings.Contains(got, "removed ") {
		t.Fatalf("unexpected fail-closed output:\n%s", got)
	}
}

func TestExecuteIsIdempotent(t *testing.T) {
	// Removing a non-existent path is not an error.
	items := []Item{
		{Kind: kindFile, Path: filepath.Join(t.TempDir(), "nope")},
		{Kind: kindDir, Path: filepath.Join(t.TempDir(), "nodir")},
	}
	if err := Execute(io.Discard, items); err != nil {
		t.Fatalf("Execute on absent paths: %v", err)
	}
}

func TestExecuteRefusesProtectedPath(t *testing.T) {
	items := []Item{{Kind: kindDir, Path: "/etc"}}
	err := Execute(io.Discard, items)
	if err == nil {
		t.Fatal("expected error for protected path, got nil")
	}
	if _, statErr := os.Stat("/etc"); statErr != nil {
		t.Errorf("/etc should still exist: %v", statErr)
	}
}

func TestExecuteRefusesUnownedCustomDirectory(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(io.Discard, []Item{{Kind: kindDir, Path: dir}}); err == nil {
		t.Fatal("expected unowned custom directory removal to be rejected")
	}
	if !pathExists(keep) {
		t.Fatal("custom directory contents should remain")
	}
}

func TestExecuteRefusesDirectoryPresentedAsFile(t *testing.T) {
	dir := t.TempDir()
	if err := Execute(io.Discard, []Item{{Kind: kindFile, Path: dir}}); err == nil {
		t.Fatal("expected directory presented as file to be rejected")
	}
	if !pathExists(dir) {
		t.Fatal("directory should remain")
	}
}

func TestExecuteManagedConfigRechecksMarker(t *testing.T) {
	t.Run("regular marker", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", root)
		cfg, err := appdirs.UserConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(cfg)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, appdirs.ManagedMarkerName)
		if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("vmflow\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Execute(io.Discard, []Item{{Kind: kindManagedConfig, Path: dir}}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if pathLexists(dir) {
			t.Fatal("managed config directory should have been removed")
		}
	})

	t.Run("marker removed after planning", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", root)
		cfg, err := appdirs.UserConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(cfg)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Execute(io.Discard, []Item{{Kind: kindManagedConfig, Path: dir}}); err == nil {
			t.Fatal("expected changed marker to block config removal")
		}
		if !pathLexists(cfg) {
			t.Fatal("config must remain after ownership validation fails")
		}
	})

	t.Run("marker replaced by symlink", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", root)
		cfg, err := appdirs.UserConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(cfg)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, appdirs.ManagedMarkerName)
		if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cfg, marker); err != nil {
			t.Fatal(err)
		}
		if err := Execute(io.Discard, []Item{{Kind: kindManagedConfig, Path: dir}}); err == nil {
			t.Fatal("expected symlink marker to block config removal")
		}
		if !pathLexists(cfg) {
			t.Fatal("config must remain after ownership validation fails")
		}
	})
}

func TestRemoveSelf(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fakebin")
	if err := os.WriteFile(f, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeSelf(f, io.Discard); err != nil {
		t.Fatalf("removeSelf: %v", err)
	}
	if pathExists(f) {
		t.Error("file should have been removed")
	}
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"anything\n", false},
	}
	for _, c := range cases {
		got, err := Confirm(io.Discard, strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("Confirm(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Confirm(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestPrintWithTranslatorLocalizesPlanAndPreservesDiagnostics(t *testing.T) {
	var output bytes.Buffer
	PrintWithTranslator(&output, []Item{
		{Kind: kindService, Note: "stop and remove native service (systemd / launchd / Windows Service)"},
		{Kind: kindBinary, Path: "/usr/local/bin/vmflow", Note: "vmflow binary", Self: true},
	}, []string{"diagnostic text"}, i18n.New(i18n.SimplifiedChinese))

	got := output.String()
	for _, want := range []string{"警告：", "diagnostic text", "将删除以下内容：", "停止并删除原生系统服务", "/usr/local/bin/vmflow", "vmflow 二进制"} {
		if !strings.Contains(got, want) {
			t.Errorf("localized uninstall plan missing %q:\n%s", want, got)
		}
	}
}

func TestConfirmWithTranslatorAcceptsChineseYes(t *testing.T) {
	var output bytes.Buffer
	confirmed, err := ConfirmWithTranslator(&output, strings.NewReader("是\n"), i18n.New(i18n.SimplifiedChinese))
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("Chinese affirmative response was not accepted")
	}
	if got := output.String(); !strings.Contains(got, "确认卸载") {
		t.Fatalf("Chinese confirmation prompt = %q", got)
	}
}

func TestPackageOwnerUnmanaged(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notapkg")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if owner := packageOwner(f); owner != "" {
		t.Errorf("packageOwner(unmanaged)=%q want empty", owner)
	}
}

func TestPlanIncludesRunningBinary(t *testing.T) {
	items, _ := Plan()
	var bin *Item
	for i := range items {
		if items[i].Self {
			bin = &items[i]
			break
		}
	}
	if bin == nil {
		t.Fatal("Plan did not include the running binary item")
	}
	if bin.Kind != kindBinary || bin.Path == "" {
		t.Errorf("binary item malformed: %+v", bin)
	}
}
