package statsstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "stats.json"))
	in := []engine.TrafficSnapshot{
		{RuleID: "r1", UploadBytes: 100, DownloadBytes: 200, SourceIPDenied: 2, UDPSessionRejected: 3, UDPPacketsDropped: 5, Conns: 9},
		{RuleID: "r2", UploadBytes: 50},
	}
	if err := store.Save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(out), out)
	}
	got := out[0]
	if got.RuleID != "r1" || got.UploadBytes != 100 || got.DownloadBytes != 200 || got.SourceIPDenied != 2 || got.UDPSessionRejected != 3 || got.UDPPacketsDropped != 5 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Conns != 0 {
		t.Fatalf("conns must not be persisted: %d", got.Conns)
	}
	if err := store.Save([]engine.TrafficSnapshot{{RuleID: "r1", UploadBytes: 300}}); err != nil {
		t.Fatalf("replace existing stats: %v", err)
	}
	replaced, err := store.Load()
	if err != nil {
		t.Fatalf("load replacement: %v", err)
	}
	if len(replaced) != 1 || replaced[0].UploadBytes != 300 {
		t.Fatalf("replacement mismatch: %+v", replaced)
	}
}

func TestSaveRenameFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "stats-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(target).Save(nil); err == nil {
		t.Fatal("saving over a directory should fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".vmflow-stats-") {
			t.Fatalf("failed save left temporary file %s", entry.Name())
		}
	}
}

func TestResolvePath(t *testing.T) {
	configPath := filepath.Join("opt", "vmflow", "config.yaml")
	if got, want := ResolvePath(configPath, "state/traffic.json", ""), filepath.Join("opt", "vmflow", "state", "traffic.json"); got != want {
		t.Fatalf("relative configured path = %q, want %q", got, want)
	}
	stateDir := filepath.Join(string(filepath.Separator), "var", "lib", "vmflow")
	if got, want := ResolvePath(configPath, "", stateDir), filepath.Join(stateDir, DefaultFilename); got != want {
		t.Fatalf("state directory path = %q, want %q", got, want)
	}
	if got, want := ResolvePath(configPath, "", ""), filepath.Join("opt", "vmflow", DefaultFilename); got != want {
		t.Fatalf("config-adjacent path = %q, want %q", got, want)
	}
}

func TestSameFilePathResolvesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "stats.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	same, err := SameFilePath(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("symlink and target were not recognized as the same file")
	}
}

func TestLoadMissingFileReturnsNil(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "absent.json"))
	out, err := store.Load()
	if err != nil || out != nil {
		t.Fatalf("missing file should return (nil, nil), got (%+v, %v)", out, err)
	}
}

func TestLoadCorruptReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path).Load(); err == nil {
		t.Fatalf("corrupt file should error")
	}
}

func TestEmptyPathIsNoOp(t *testing.T) {
	store := New("")
	if err := store.Save(nil); err != nil {
		t.Fatalf("save empty path: %v", err)
	}
	if out, err := store.Load(); err != nil || out != nil {
		t.Fatalf("load empty path: (%+v, %v)", out, err)
	}
}
