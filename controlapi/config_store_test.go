package controlapi

import (
	"bytes"
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

func TestBuildCandidatePreservesRuleLevelYAML(t *testing.T) {
	raw := []byte(`version: 1
udp_max_sessions: 64
rule_defaults: &rule_defaults
  custom_inherited: keep
rules: # keep rules comment
  # keep first rule comment
  - rule_id: first # keep rule id comment
    name: First
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: &first_port 21001 # keep port comment
    custom_port_copy: *first_port
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
    custom_extension: # keep extension comment
      nested: keep
  - <<: *rule_defaults
    rule_id: second
    name: Second
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 21002
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
`)
	path := writeConfigStoreFixture(t, raw, 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	desired := append([]engine.Rule(nil), document.Config.Rules...)
	desired[0].ListenPort = 21003
	desired[0].Remark = "updated"
	desired[1].Name = "Second updated"
	desired[0], desired[1] = desired[1], desired[0]

	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          desired,
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatalf("BuildCandidate: %v", err)
	}
	rulesNode := testYAMLMappingValue(t, testYAMLRoot(t, candidate.raw), "rules")
	if rulesNode.Kind != yaml.SequenceNode || len(rulesNode.Content) != 2 {
		t.Fatalf("rules node = kind %d with %d items", rulesNode.Kind, len(rulesNode.Content))
	}
	if got := []string{testYAMLRuleID(t, rulesNode.Content[0]), testYAMLRuleID(t, rulesNode.Content[1])}; !reflect.DeepEqual(got, []string{"second", "first"}) {
		t.Fatalf("rule order = %v", got)
	}

	second := rulesNode.Content[0]
	merge := testYAMLMappingValue(t, second, "<<")
	if merge.Kind != yaml.AliasNode || merge.Alias == nil || merge.Alias.Anchor != "rule_defaults" {
		t.Fatalf("second rule merge node was not preserved: %+v", merge)
	}
	first := rulesNode.Content[1]
	if extension := testYAMLMappingValue(t, first, "custom_extension"); extension.Kind != yaml.MappingNode {
		t.Fatalf("custom_extension = %+v", extension)
	}
	port := testYAMLMappingValue(t, first, "listen_port")
	portCopy := testYAMLMappingValue(t, first, "custom_port_copy")
	if port.Value != "21003" || port.Anchor != "first_port" || !strings.Contains(port.LineComment, "keep port comment") {
		t.Fatalf("listen_port presentation was not preserved: %+v", port)
	}
	if portCopy.Kind != yaml.AliasNode || portCopy.Alias != port {
		t.Fatalf("custom_port_copy no longer aliases listen_port: %+v", portCopy)
	}
	if ruleID := testYAMLMappingValue(t, first, "rule_id"); !strings.Contains(ruleID.LineComment, "keep rule id comment") {
		t.Fatalf("rule_id comment was not preserved: %+v", ruleID)
	}
	for _, comment := range []string{"# keep rules comment", "# keep first rule comment", "# keep extension comment"} {
		if !strings.Contains(string(candidate.raw), comment) {
			t.Errorf("candidate omitted %q:\n%s", comment, candidate.raw)
		}
	}
	if candidate.Config.Rules[0].RuleID != "second" || candidate.Config.Rules[0].Name != "Second updated" ||
		candidate.Config.Rules[1].RuleID != "first" || candidate.Config.Rules[1].ListenPort != 21003 || candidate.Config.Rules[1].Remark != "updated" {
		t.Fatalf("candidate rules = %+v", candidate.Config.Rules)
	}
}

func TestBuildCandidatePreservesOptionalRuleFieldCommentsWhenCleared(t *testing.T) {
	raw := []byte(`version: 1
udp_max_sessions: 64
rules:
  - rule_id: optional
    name: Optional
    protocol: tcp
    listen_addr: 127.0.0.1
    listen_port: 21004
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
    source_ip_mode: allowlist # keep mode comment
    source_ips: # keep source list comment
      - 127.0.0.1
    remark: remove me # keep remark comment
`)
	path := writeConfigStoreFixture(t, raw, 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	desired := append([]engine.Rule(nil), document.Config.Rules...)
	desired[0].SourceIPMode = ""
	desired[0].SourceIPs = nil
	desired[0].Remark = ""

	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          desired,
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatalf("BuildCandidate: %v", err)
	}
	if rule := candidate.Config.Rules[0]; rule.SourceIPMode != "" || len(rule.SourceIPs) != 0 || rule.Remark != "" {
		t.Fatalf("cleared rule fields = %+v", rule)
	}
	rulesNode := testYAMLMappingValue(t, testYAMLRoot(t, candidate.raw), "rules")
	ruleNode := rulesNode.Content[0]
	mode := testYAMLMappingValue(t, ruleNode, "source_ip_mode")
	sources := testYAMLMappingValue(t, ruleNode, "source_ips")
	remark := testYAMLMappingValue(t, ruleNode, "remark")
	if mode.Value != "" || !strings.Contains(mode.LineComment, "keep mode comment") {
		t.Fatalf("source_ip_mode = %+v", mode)
	}
	if sources.Kind != yaml.SequenceNode || len(sources.Content) != 0 || !strings.Contains(string(candidate.raw), "keep source list comment") {
		t.Fatalf("source_ips = %+v\n%s", sources, candidate.raw)
	}
	if remark.Value != "" || !strings.Contains(remark.LineComment, "keep remark comment") {
		t.Fatalf("remark = %+v", remark)
	}
}

func TestBuildCandidateUpdatesAnchoredManagedScalar(t *testing.T) {
	raw := []byte(`version: 1
udp_max_sessions: &session_limit 64 # keep session comment
custom_session_limit: *session_limit
rules: []
`)
	path := writeConfigStoreFixture(t, raw, 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{Rules: document.Config.Rules, UDPMaxSessions: 128})
	if err != nil {
		t.Fatalf("BuildCandidate: %v", err)
	}
	root := testYAMLRoot(t, candidate.raw)
	limit := testYAMLMappingValue(t, root, "udp_max_sessions")
	alias := testYAMLMappingValue(t, root, "custom_session_limit")
	if limit.Value != "128" || limit.Anchor != "session_limit" || !strings.Contains(limit.LineComment, "keep session comment") {
		t.Fatalf("udp_max_sessions = %+v", limit)
	}
	if alias.Kind != yaml.AliasNode || alias.Alias != limit {
		t.Fatalf("custom_session_limit no longer aliases udp_max_sessions: %+v", alias)
	}
}

func TestBuildCandidateNoopPreservesOriginalBytes(t *testing.T) {
	raw := []byte(`# keep exact document
version: 1

custom_extension: {answer: 42}
udp_max_sessions: 64
rules: [] # keep empty rules
`)
	path := writeConfigStoreFixture(t, raw, 0o600)
	document, err := loadConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := document.BuildCandidate(rulesConfigDraft{
		Rules:          document.Config.Rules,
		UDPMaxSessions: document.Config.UDPMaxSessions,
	})
	if err != nil {
		t.Fatalf("BuildCandidate: %v", err)
	}
	if !bytes.Equal(candidate.raw, raw) {
		t.Fatalf("no-op candidate changed original bytes:\n%s", candidate.raw)
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

func testYAMLRoot(t *testing.T, raw []byte) *yaml.Node {
	t.Helper()
	document, err := parseYAMLDocument(raw)
	if err != nil {
		t.Fatalf("parse YAML document: %v\n%s", err, raw)
	}
	return document.Content[0]
}

func testYAMLMappingValue(t *testing.T, mapping *yaml.Node, key string) *yaml.Node {
	t.Helper()
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		t.Fatalf("YAML node for %q is not a mapping: %+v", key, mapping)
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	t.Fatalf("YAML mapping does not contain %q: %+v", key, mapping)
	return nil
}

func testYAMLRuleID(t *testing.T, node *yaml.Node) string {
	t.Helper()
	var identity struct {
		RuleID string `yaml:"rule_id"`
	}
	if err := node.Decode(&identity); err != nil {
		t.Fatalf("decode YAML rule identity: %v", err)
	}
	return strings.TrimSpace(identity.RuleID)
}
