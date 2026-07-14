// Package statsstore persists per-rule cumulative traffic counters to a JSON
// file with atomic writes, so daemon restarts do not lose upload/download/drop
// totals. It only stores cumulative values — live connection counts and rates
// are not persisted.
package statsstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cloudapp3/vmflow/engine"
)

const documentVersion = 1

// DefaultFilename is the state filename used when no explicit path is set.
const DefaultFilename = "stats.json"

// Store reads and writes cumulative traffic counters at path.
type Store struct {
	path string
	mu   sync.Mutex
}

// New returns a store backed by path. An empty path makes Save/Load no-ops.
func New(path string) *Store {
	path = strings.TrimSpace(path)
	if path == "" {
		return &Store{path: ""}
	}
	return &Store{path: filepath.Clean(path)}
}

// ResolvePath returns the stats file path. Explicit relative paths are resolved
// beside the config file; otherwise a service state directory takes precedence
// over the config directory.
func ResolvePath(configPath, configuredPath, stateDirectory string) string {
	if configuredPath = strings.TrimSpace(configuredPath); configuredPath != "" {
		if filepath.IsAbs(configuredPath) {
			return filepath.Clean(configuredPath)
		}
		return filepath.Clean(filepath.Join(filepath.Dir(configPath), configuredPath))
	}
	if stateDirectory = strings.TrimSpace(stateDirectory); stateDirectory != "" && filepath.IsAbs(stateDirectory) {
		return filepath.Join(filepath.Clean(stateDirectory), DefaultFilename)
	}
	return filepath.Join(filepath.Dir(configPath), DefaultFilename)
}

// SameFilePath reports whether two paths identify the same destination. It
// handles existing hard links and symlinks, plus not-yet-created files whose
// parent directory can be resolved.
func SameFilePath(left, right string) (bool, error) {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false, fmt.Errorf("compare paths: path is empty")
	}
	leftPath, err := canonicalPath(left)
	if err != nil {
		return false, fmt.Errorf("compare path %s: %w", left, err)
	}
	rightPath, err := canonicalPath(right)
	if err != nil {
		return false, fmt.Errorf("compare path %s: %w", right, err)
	}
	if pathsEqual(leftPath, rightPath) {
		return true, nil
	}
	leftInfo, leftErr := os.Stat(leftPath)
	rightInfo, rightErr := os.Stat(rightPath)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo), nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err == nil {
		return filepath.Join(parent, filepath.Base(absolute)), nil
	}
	return filepath.Clean(absolute), nil
}

func pathsEqual(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// Path returns the configured file path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

type record struct {
	RuleID             string `json:"rule_id"`
	UploadBytes        int64  `json:"upload_bytes"`
	DownloadBytes      int64  `json:"download_bytes"`
	UDPSessionRejected int64  `json:"udp_session_rejected,omitempty"`
	UDPPacketsDropped  int64  `json:"udp_packets_dropped,omitempty"`
}

type document struct {
	Version   int      `json:"version"`
	UpdatedAt int64    `json:"updated_at"`
	Rules     []record `json:"rules"`
}

func toRecords(snapshots []engine.TrafficSnapshot) []record {
	records := make([]record, 0, len(snapshots))
	for _, s := range snapshots {
		records = append(records, record{
			RuleID:             s.RuleID,
			UploadBytes:        s.UploadBytes,
			DownloadBytes:      s.DownloadBytes,
			UDPSessionRejected: s.UDPSessionRejected,
			UDPPacketsDropped:  s.UDPPacketsDropped,
		})
	}
	return records
}

func fromRecords(records []record) []engine.TrafficSnapshot {
	snapshots := make([]engine.TrafficSnapshot, 0, len(records))
	for _, r := range records {
		snapshots = append(snapshots, engine.TrafficSnapshot{
			RuleID:             r.RuleID,
			UploadBytes:        r.UploadBytes,
			DownloadBytes:      r.DownloadBytes,
			UDPSessionRejected: r.UDPSessionRejected,
			UDPPacketsDropped:  r.UDPPacketsDropped,
		})
	}
	return snapshots
}

// Save atomically writes the cumulative counters (temp file + fsync + rename).
func (s *Store) Save(snapshots []engine.TrafficSnapshot) error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := document{Version: documentVersion, UpdatedAt: time.Now().Unix(), Rules: toRecords(snapshots)}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create stats dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".vmflow-stats-*")
	if err != nil {
		return fmt.Errorf("create temp stats file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write stats: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync stats: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close stats: %w", err)
	}
	if err := replaceStatsFile(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace stats file: %w", err)
	}
	if err := syncStatsDirectory(dir); err != nil {
		return fmt.Errorf("sync stats directory: %w", err)
	}
	return nil
}

// Load reads persisted counters. A missing file returns (nil, nil) (first run).
// A corrupt or unsupported file returns an error so the caller can log it and
// start from zero rather than silently losing data.
func (s *Store) Load() ([]engine.TrafficSnapshot, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read stats: %w", err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse stats: %w", err)
	}
	if doc.Version != documentVersion {
		return nil, fmt.Errorf("unsupported stats version %d", doc.Version)
	}
	return fromRecords(doc.Rules), nil
}
