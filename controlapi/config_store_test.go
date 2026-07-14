package controlapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/engine"
	"gopkg.in/yaml.v3"
)

func TestLoadConfigDocumentRevision(t *testing.T) {
	raw := []byte("version: 1\nudp_max_sessions: 64\nrules: []\n")
	path := writeConfigStoreFixture(t, raw, 0o640)

	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatalf("loadConfigDocument: %v", err)
	}
	wantRevision := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if document.Revision != wantRevision {
		t.Fatalf("Revision = %q, want %q", document.Revision, wantRevision)
	}
	if document.Config.UDPMaxSessions != 64 || len(document.Config.Rules) != 0 {
		t.Fatalf("Config = %+v", document.Config)
	}
}

func TestBuildCandidatePreservesUnmanagedYAML(t *testing.T) {
	raw := []byte(`# document comment
version: 1
custom_extension: # keep extension comment
  answer: 42
auth:
  enabled: false
  tokens:
    - name: retained
      token: do-not-drop
      role: viewer
udp_max_sessions: 64 # keep UDP comment
rules: # managed rules comment
  - rule_id: old
    name: old
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 21001
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
`)
	path := writeConfigStoreFixture(t, raw, 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}

	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule(" new ", 21002, true)},
		UDPMaxSessions: 512,
	})
	if err != nil {
		t.Fatalf("BuildCandidate: %v", err)
	}
	if candidate.Config.UDPMaxSessions != 512 {
		t.Fatalf("UDPMaxSessions = %d, want 512", candidate.Config.UDPMaxSessions)
	}
	if len(candidate.Config.Rules) != 1 || candidate.Config.Rules[0].RuleID != "new" {
		t.Fatalf("Rules = %+v", candidate.Config.Rules)
	}
	for _, comment := range []string{
		"# document comment",
		"# keep extension comment",
		"# keep UDP comment",
		"# managed rules comment",
	} {
		if !strings.Contains(string(candidate.raw), comment) {
			t.Errorf("candidate omitted comment %q:\n%s", comment, candidate.raw)
		}
	}

	var decoded map[string]any
	if err := yaml.Unmarshal(candidate.raw, &decoded); err != nil {
		t.Fatalf("decode candidate: %v", err)
	}
	extension, ok := decoded["custom_extension"].(map[string]any)
	if !ok || extension["answer"] != 42 {
		t.Fatalf("custom_extension = %#v", decoded["custom_extension"])
	}
	auth, ok := decoded["auth"].(map[string]any)
	if !ok || auth["tokens"] == nil {
		t.Fatalf("auth was not preserved: %#v", decoded["auth"])
	}
	if _, found := strings.CutPrefix(candidate.Revision, "sha256:"); !found {
		t.Fatalf("candidate Revision = %q", candidate.Revision)
	}
}

func TestConfigCandidateStageCommitPreservesMetadata(t *testing.T) {
	raw := []byte("version: 1\nudp_max_sessions: 64\nrules: []\n")
	path := writeConfigStoreFixture(t, raw, 0o640)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeOwner := configStoreOwnerFingerprint(before)

	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("committed", 21003, false)},
		UDPMaxSessions: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := candidate.Stage()
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer staged.Discard()
	if _, err := os.Stat(staged.tempPath); err != nil {
		t.Fatalf("staged temporary file: %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(raw) {
		t.Fatalf("Stage changed target file:\n%s", unchanged)
	}

	if err := staged.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !staged.Committed() {
		t.Fatal("Committed() = false after successful Commit")
	}
	if err := staged.Discard(); err != nil {
		t.Fatalf("Discard after Commit: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("mode = %o, want %o", after.Mode().Perm(), before.Mode().Perm())
	}
	if beforeOwner != "" && configStoreOwnerFingerprint(after) != beforeOwner {
		t.Fatalf("owner changed from %s to %s", beforeOwner, configStoreOwnerFingerprint(after))
	}

	loaded, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != candidate.Revision {
		t.Fatalf("committed Revision = %q, want %q", loaded.Revision, candidate.Revision)
	}
	if loaded.Config.UDPMaxSessions != 128 || len(loaded.Config.Rules) != 1 {
		t.Fatalf("committed Config = %+v", loaded.Config)
	}
}

func TestStagedConfigCommitUsesLatestTargetMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows permissions are verified through the copied security descriptor")
	}
	path := writeConfigStoreFixture(t, []byte("version: 1\nrules: []\n"), 0o640)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("metadata", 21008, false)},
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := candidate.Stage()
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.Commit(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("committed mode = %o, want latest target mode 600", info.Mode().Perm())
	}
}

func TestConfigCandidateDiscardLeavesOriginal(t *testing.T) {
	raw := []byte("version: 1\nrules: []\n")
	path := writeConfigStoreFixture(t, raw, 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("discarded", 21004, false)},
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := candidate.Stage()
	if err != nil {
		t.Fatal(err)
	}
	tempPath := staged.tempPath
	if err := staged.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if staged.Committed() {
		t.Fatal("Committed() = true after Discard")
	}
	if err := staged.Discard(); err != nil {
		t.Fatalf("second Discard: %v", err)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file still exists: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("Discard changed original:\n%s", got)
	}
}

func TestStagedConfigCommitReportsPostRenameSyncFailureAsCommitted(t *testing.T) {
	path := writeConfigStoreFixture(t, []byte("version: 1\nrules: []\n"), 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("durability", 21007, false)},
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := candidate.Stage()
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	staged.syncDirectory = func(string) error { return errors.New("injected directory sync failure") }

	err = staged.Commit()
	var commitErr *configCommitError
	if !errors.As(err, &commitErr) || !commitErr.Committed {
		t.Fatalf("Commit error = %v, want committed configCommitError", err)
	}
	if !staged.Committed() {
		t.Fatal("Committed() = false after successful rename")
	}
	loaded, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != candidate.Revision {
		t.Fatalf("on-disk Revision = %q, want candidate %q", loaded.Revision, candidate.Revision)
	}
}

func TestConfigCandidateStageDetectsRevisionConflict(t *testing.T) {
	path := writeConfigStoreFixture(t, []byte("version: 1\nrules: []\n"), 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("candidate", 21005, false)},
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\n# external edit\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.Stage(); !errors.Is(err, errConfigRevisionConflict) {
		t.Fatalf("Stage error = %v, want revision conflict", err)
	}
}

func TestConfigCandidateStageTreatsSymlinkReplacementAsRevisionConflict(t *testing.T) {
	path := writeConfigStoreFixture(t, []byte("version: 1\nrules: []\n"), 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("candidate", 21008, false)},
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(filepath.Dir(path), "replacement-real.yaml")
	if err := os.WriteFile(realPath, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := candidate.Stage(); !errors.Is(err, errConfigRevisionConflict) {
		t.Fatalf("Stage error = %v, want revision conflict", err)
	}
}

func TestStagedConfigCommitDetectsExternalReplacement(t *testing.T) {
	path := writeConfigStoreFixture(t, []byte("version: 1\nrules: []\n"), 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          []engine.Rule{configStoreRule("candidate", 21006, false)},
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := candidate.Stage()
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()

	replacement := filepath.Join(filepath.Dir(path), "replacement.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceConfigFile(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := staged.Commit(); !errors.Is(err, errConfigRevisionConflict) {
		t.Fatalf("Commit error = %v, want revision conflict", err)
	}
}

func TestLoadConfigDocumentRejectsSymlinkAndNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(realPath, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Logf("symlink unavailable: %v", err)
	} else if _, err := loadConfigDocument(symlinkPath); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink load error = %v", err)
	}
	if _, err := loadConfigDocument(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory load error = %v", err)
	}
}

func writeConfigStoreFixture(t *testing.T, raw []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func configStoreRule(ruleID string, listenPort int, enabled bool) engine.Rule {
	return engine.Rule{
		RuleID:     ruleID,
		Name:       strings.TrimSpace(ruleID),
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: listenPort,
		TargetAddr: "127.0.0.1",
		TargetPort: 22,
		Enabled:    enabled,
	}
}

func configStoreOwnerFingerprint(info os.FileInfo) string {
	if info == nil || info.Sys() == nil {
		return ""
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	uid := value.FieldByName("Uid")
	gid := value.FieldByName("Gid")
	if !uid.IsValid() || !gid.IsValid() || !uid.CanUint() || !gid.CanUint() {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid.Uint(), gid.Uint())
}
