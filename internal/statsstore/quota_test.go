package statsstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
)

func TestQuotaStoreRoundTripAndStrictCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultQuotaFilename)
	store := NewQuotaStore(path)
	want := []engine.QuotaPersistentState{{
		RuleID: "limited", Daily: map[string]int64{"2026-08-02": 100}, Monthly: map[string]int64{"2026-08": 200},
	}}
	if err := store.SaveQuotaState(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadQuotaState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Daily["2026-08-02"] != 100 || got[0].Monthly["2026-08"] != 200 {
		t.Fatalf("quota state = %+v", got)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadQuotaState(); err == nil {
		t.Fatal("corrupt quota state should fail")
	}
}
