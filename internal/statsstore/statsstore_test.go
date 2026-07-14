package statsstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "stats.json"))
	in := []engine.TrafficSnapshot{
		{RuleID: "r1", UploadBytes: 100, DownloadBytes: 200, UDPSessionRejected: 3, UDPPacketsDropped: 5, Conns: 9},
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
	if got.RuleID != "r1" || got.UploadBytes != 100 || got.DownloadBytes != 200 || got.UDPSessionRejected != 3 || got.UDPPacketsDropped != 5 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Conns != 0 {
		t.Fatalf("conns must not be persisted: %d", got.Conns)
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
