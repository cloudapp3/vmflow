package updater

import (
	"path/filepath"
	"testing"
)

func TestCacheFilePathTargetsOnlyUpdateCheckFile(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	want := filepath.Join(cacheRoot, "vmflow", cacheFileName)
	if got := CacheFilePath(); got != want {
		t.Fatalf("CacheFilePath() = %q, want %q", got, want)
	}
}
