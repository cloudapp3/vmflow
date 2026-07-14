//go:build linux || darwin

package uninstaller

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestColocatedConfigRequiresOwnershipMarker(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "vmflow")
	configPath := filepath.Join(dir, colocatedConfigName)
	markerPath := filepath.Join(dir, colocatedConfigMarkerName)
	if err := os.WriteFile(configPath, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gotConfig, gotMarker, owned := colocatedConfig(binaryPath)
	if gotConfig != configPath || gotMarker != markerPath || owned {
		t.Fatalf("colocatedConfig without marker = (%q, %q, %v)", gotConfig, gotMarker, owned)
	}
	if err := os.WriteFile(markerPath, []byte("vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, owned = colocatedConfig(binaryPath)
	if !owned {
		t.Fatal("regular ownership marker was not recognized")
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(configPath, markerPath); err != nil {
		t.Fatal(err)
	}
	_, _, owned = colocatedConfig(binaryPath)
	if owned {
		t.Fatal("symlink ownership marker must not be trusted")
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

func TestExecuteOwnedConfigRechecksMarker(t *testing.T) {
	t.Run("regular marker", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, colocatedConfigName)
		marker := filepath.Join(dir, colocatedConfigMarkerName)
		if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("vmflow\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Execute(io.Discard, []Item{{Kind: kindOwnedConfig, Path: cfg}}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if pathLexists(cfg) {
			t.Fatal("owned config should have been removed")
		}
	})

	t.Run("marker removed after planning", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, colocatedConfigName)
		if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Execute(io.Discard, []Item{{Kind: kindOwnedConfig, Path: cfg}}); err == nil {
			t.Fatal("expected changed marker to block config removal")
		}
		if !pathLexists(cfg) {
			t.Fatal("config must remain after ownership validation fails")
		}
	})

	t.Run("marker replaced by symlink", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, colocatedConfigName)
		marker := filepath.Join(dir, colocatedConfigMarkerName)
		if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cfg, marker); err != nil {
			t.Fatal(err)
		}
		if err := Execute(io.Discard, []Item{{Kind: kindOwnedConfig, Path: cfg}}); err == nil {
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
