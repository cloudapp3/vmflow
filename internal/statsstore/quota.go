package statsstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudapp3/vmflow/engine"
)

const quotaDocumentVersion = 1

// DefaultQuotaFilename is kept separate from cumulative counters because quota
// enforcement treats corrupt or unwritable state as fatal rather than resetting.
const DefaultQuotaFilename = "quota-state.json"

type QuotaStore struct {
	path string
	mu   sync.Mutex
}

func NewQuotaStore(path string) *QuotaStore {
	return &QuotaStore{path: filepath.Clean(strings.TrimSpace(path))}
}

func (store *QuotaStore) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

type quotaDocument struct {
	Version   int                           `json:"version"`
	UpdatedAt int64                         `json:"updated_at"`
	Rules     []engine.QuotaPersistentState `json:"rules"`
}

func (store *QuotaStore) LoadQuotaState() ([]engine.QuotaPersistentState, error) {
	if store == nil || store.path == "" || store.path == "." {
		return nil, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	data, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read quota state: %w", err)
	}
	var document quotaDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse quota state: %w", err)
	}
	if document.Version != quotaDocumentVersion {
		return nil, fmt.Errorf("unsupported quota state version %d", document.Version)
	}
	return document.Rules, nil
}

func (store *QuotaStore) SaveQuotaState(records []engine.QuotaPersistentState) error {
	if store == nil || store.path == "" || store.path == "." {
		return fmt.Errorf("quota state path is empty")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	document := quotaDocument{Version: quotaDocumentVersion, UpdatedAt: time.Now().Unix(), Rules: records}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal quota state: %w", err)
	}
	dir := filepath.Dir(store.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create quota state dir: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".vmflow-quota-*")
	if err != nil {
		return fmt.Errorf("create temporary quota state: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write quota state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync quota state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close quota state: %w", err)
	}
	if err := replaceStatsFile(temporaryPath, store.path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("replace quota state: %w", err)
	}
	if err := syncStatsDirectory(dir); err != nil {
		return fmt.Errorf("sync quota state directory: %w", err)
	}
	return nil
}
