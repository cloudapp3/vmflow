package controlapi

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
	"gopkg.in/yaml.v3"
)

var errConfigRevisionConflict = errors.New("config revision conflict")

var errConfigPathChanged = errors.New("config path changed")

// configCommitError reports an error after the atomic rename commit point. If
// Committed is true, callers must not treat the old on-disk config as current.
type configCommitError struct {
	Committed bool
	Err       error
}

func (err *configCommitError) Error() string {
	return err.Err.Error()
}

func (err *configCommitError) Unwrap() error {
	return err.Err
}

// rulesConfigDraft is the complete hot-reloadable configuration managed by the
// rules API. Callers that only change rules must carry forward UDPMaxSessions
// from the loaded config document.
type rulesConfigDraft struct {
	Rules          []engine.Rule
	UDPMaxSessions int
}

// configDocument is one validated snapshot of the config file. Revision hashes
// the original bytes, so formatting-only external edits also prevent a stale
// management request from overwriting the file.
type configDocument struct {
	Config   config.File
	Revision string

	path string
	raw  []byte
}

// configCandidate contains a validated replacement document. Only the top-level
// rules and udp_max_sessions nodes differ from its source document.
type configCandidate struct {
	Config   config.File
	Revision string

	path           string
	raw            []byte
	sourceRevision string
}

// stagedConfig is a synced same-directory temporary file awaiting the atomic
// rename commit point.
type stagedConfig struct {
	mu sync.Mutex

	path           string
	tempPath       string
	sourceRevision string
	targetInfo     os.FileInfo
	tempInfo       os.FileInfo
	syncDirectory  func(string) error
	done           bool
	committed      bool
}

// loadConfigDocument loads and validates path without following a symlink at
// the final path component.
func loadConfigDocument(path string) (*configDocument, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("load config document: missing config path")
	}

	raw, _, err := readRegularConfig(path)
	if err != nil {
		return nil, fmt.Errorf("load config document: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("load config document: parse config: %w", err)
	}
	if _, err := parseYAMLDocument(raw); err != nil {
		return nil, fmt.Errorf("load config document: parse YAML document: %w", err)
	}

	return &configDocument{
		Config:   cfg,
		Revision: configRevision(raw),
		path:     path,
		raw:      append([]byte(nil), raw...),
	}, nil
}

// BuildCandidate constructs and validates a complete replacement document.
// Unmanaged YAML nodes and their comments are retained by editing the YAML AST
// rather than marshaling config.File.
func (document *configDocument) BuildCandidate(draft rulesConfigDraft) (*configCandidate, error) {
	if document == nil || strings.TrimSpace(document.path) == "" || len(document.raw) == 0 {
		return nil, fmt.Errorf("build config candidate: invalid source document")
	}

	// Parse and render twice. The first pass validates and standardizes draft
	// rules through config.Parse; the second persists those normalized values.
	source := rulesConfigDraft{
		Rules:          document.Config.Rules,
		UDPMaxSessions: document.Config.UDPMaxSessions,
	}
	firstRaw, err := renderManagedConfig(document.raw, source, draft)
	if err != nil {
		return nil, fmt.Errorf("build config candidate: %w", err)
	}
	firstConfig, err := config.Parse(firstRaw)
	if err != nil {
		return nil, fmt.Errorf("build config candidate: validate candidate: %w", err)
	}

	normalized := rulesConfigDraft{
		Rules:          firstConfig.Rules,
		UDPMaxSessions: firstConfig.UDPMaxSessions,
	}
	raw, err := renderManagedConfig(document.raw, source, normalized)
	if err != nil {
		return nil, fmt.Errorf("build config candidate: render normalized candidate: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("build config candidate: validate normalized candidate: %w", err)
	}

	return &configCandidate{
		Config:         cfg,
		Revision:       configRevision(raw),
		path:           document.path,
		raw:            raw,
		sourceRevision: document.Revision,
	}, nil
}

// Stage writes and syncs a same-directory temporary file. It refuses to stage
// over a config changed since the source document was loaded.
func (candidate *configCandidate) Stage() (*stagedConfig, error) {
	if candidate == nil || strings.TrimSpace(candidate.path) == "" || len(candidate.raw) == 0 {
		return nil, fmt.Errorf("stage config candidate: invalid candidate")
	}

	currentRaw, targetInfo, err := readRegularConfig(candidate.path)
	if err != nil {
		if isConfigPathChange(err) {
			return nil, fmt.Errorf("stage config candidate: %w: %w", errConfigRevisionConflict, err)
		}
		return nil, fmt.Errorf("stage config candidate: %w", err)
	}
	if currentRevision := configRevision(currentRaw); currentRevision != candidate.sourceRevision {
		return nil, fmt.Errorf("stage config candidate: %w: expected %s, found %s", errConfigRevisionConflict, candidate.sourceRevision, currentRevision)
	}

	dir := filepath.Dir(candidate.path)
	temp, err := os.CreateTemp(dir, ".vmflow-config-*")
	if err != nil {
		return nil, fmt.Errorf("stage config candidate: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}

	if _, err := temp.Write(candidate.raw); err != nil {
		cleanup()
		return nil, fmt.Errorf("stage config candidate: write temporary file: %w", err)
	}
	if err := preserveConfigMetadata(temp, tempPath, candidate.path, targetInfo); err != nil {
		cleanup()
		return nil, fmt.Errorf("stage config candidate: preserve config metadata: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("stage config candidate: sync temporary file: %w", err)
	}
	tempInfo, err := temp.Stat()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("stage config candidate: inspect temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("stage config candidate: close temporary file: %w", err)
	}

	return &stagedConfig{
		path:           candidate.path,
		tempPath:       tempPath,
		sourceRevision: candidate.sourceRevision,
		targetInfo:     targetInfo,
		tempInfo:       tempInfo,
		syncDirectory:  syncConfigDirectory,
	}, nil
}

// Commit atomically replaces the config and syncs the parent directory. A
// revision or file-identity change after Stage is reported as a conflict.
func (staged *stagedConfig) Commit() error {
	if staged == nil {
		return fmt.Errorf("commit staged config: invalid staged config")
	}
	staged.mu.Lock()
	defer staged.mu.Unlock()
	if staged.done || staged.tempPath == "" {
		return fmt.Errorf("commit staged config: staged config is already closed")
	}

	currentRaw, currentInfo, err := readRegularConfig(staged.path)
	if err != nil {
		if isConfigPathChange(err) {
			return fmt.Errorf("commit staged config: %w: %w", errConfigRevisionConflict, err)
		}
		return fmt.Errorf("commit staged config: %w", err)
	}
	currentRevision := configRevision(currentRaw)
	if currentRevision != staged.sourceRevision || !os.SameFile(staged.targetInfo, currentInfo) {
		return fmt.Errorf("commit staged config: %w: config changed after staging", errConfigRevisionConflict)
	}
	refreshedTempInfo, err := refreshStagedConfigMetadata(staged.tempPath, staged.path, staged.tempInfo, currentInfo)
	if err != nil {
		return fmt.Errorf("commit staged config: refresh config metadata: %w", err)
	}
	staged.tempInfo = refreshedTempInfo
	tempInfo, err := os.Lstat(staged.tempPath)
	if err != nil {
		return fmt.Errorf("commit staged config: inspect temporary file: %w", err)
	}
	if !tempInfo.Mode().IsRegular() || !os.SameFile(staged.tempInfo, tempInfo) {
		return fmt.Errorf("commit staged config: temporary file changed after staging")
	}
	if err := replaceConfigFile(staged.tempPath, staged.path); err != nil {
		return fmt.Errorf("commit staged config: replace config file: %w", err)
	}

	staged.done = true
	staged.committed = true
	staged.tempPath = ""
	if err := staged.syncDirectory(filepath.Dir(staged.path)); err != nil {
		return &configCommitError{
			Committed: true,
			Err:       fmt.Errorf("commit staged config: config replaced but parent directory sync failed: %w", err),
		}
	}
	return nil
}

func refreshStagedConfigMetadata(tempPath, targetPath string, expectedTempInfo, targetInfo os.FileInfo) (os.FileInfo, error) {
	pathInfo, err := os.Lstat(tempPath)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(expectedTempInfo, pathInfo) {
		return nil, fmt.Errorf("temporary file changed before metadata refresh")
	}
	temp, err := os.OpenFile(tempPath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer temp.Close()
	openedInfo, err := temp.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(expectedTempInfo, openedInfo) {
		return nil, fmt.Errorf("temporary file changed while refreshing metadata")
	}
	if err := preserveConfigMetadata(temp, tempPath, targetPath, targetInfo); err != nil {
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		return nil, err
	}
	return temp.Stat()
}

// Committed reports whether the atomic replacement already occurred. It is
// useful when handling a post-rename durability error from Commit.
func (staged *stagedConfig) Committed() bool {
	if staged == nil {
		return false
	}
	staged.mu.Lock()
	defer staged.mu.Unlock()
	return staged.committed
}

// Discard removes an uncommitted temporary file. It is idempotent so callers
// can defer it immediately after a successful Stage.
func (staged *stagedConfig) Discard() error {
	if staged == nil {
		return nil
	}
	staged.mu.Lock()
	defer staged.mu.Unlock()
	if staged.done || staged.tempPath == "" {
		return nil
	}
	err := os.Remove(staged.tempPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discard staged config: %w", err)
	}
	staged.done = true
	staged.tempPath = ""
	return nil
}

func readRegularConfig(path string) ([]byte, os.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%w: config path is a symlink", errConfigPathChanged)
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: config path is not a regular file", errConfigPathChanged)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, nil, fmt.Errorf("%w while opening", errConfigPathChanged)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	return raw, openedInfo, nil
}

func isConfigPathChange(err error) bool {
	return errors.Is(err, errConfigPathChanged) || errors.Is(err, os.ErrNotExist)
}

func configRevision(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", sum)
}

func renderManagedConfig(source []byte, current, desired rulesConfigDraft) ([]byte, error) {
	rulesChanged := !rulesYAMLEqual(current.Rules, desired.Rules)
	udpChanged := current.UDPMaxSessions != desired.UDPMaxSessions
	if !rulesChanged && !udpChanged {
		return append([]byte(nil), source...), nil
	}

	document, err := parseYAMLDocument(source)
	if err != nil {
		return nil, err
	}
	root := document.Content[0]

	if rulesChanged {
		if err := mergeRulesYAML(root, current.Rules, desired.Rules); err != nil {
			return nil, fmt.Errorf("merge rules: %w", err)
		}
	}
	if udpChanged {
		udpNode, err := encodeYAMLNode(desired.UDPMaxSessions)
		if err != nil {
			return nil, fmt.Errorf("encode udp_max_sessions: %w", err)
		}
		setTopLevelYAMLNode(root, "udp_max_sessions", udpNode)
	}

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type ruleYAMLField struct {
	name  string
	index int
}

var managedRuleYAMLFields = func() []ruleYAMLField {
	ruleType := reflect.TypeOf(engine.Rule{})
	fields := make([]ruleYAMLField, 0, ruleType.NumField())
	for index := 0; index < ruleType.NumField(); index++ {
		field := ruleType.Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fields = append(fields, ruleYAMLField{name: name, index: index})
	}
	return fields
}()

func rulesYAMLEqual(current, desired []engine.Rule) bool {
	if len(current) != len(desired) {
		return false
	}
	for index := range current {
		if !reflect.DeepEqual(current[index], desired[index]) {
			return false
		}
	}
	return true
}

func mergeRulesYAML(root *yaml.Node, current, desired []engine.Rule) error {
	rulesNode, found := yamlMappingValue(root, "rules")
	if !found {
		replacement, err := encodeYAMLNode(nonNilRules(desired))
		if err != nil {
			return fmt.Errorf("encode rules: %w", err)
		}
		setTopLevelYAMLNode(root, "rules", replacement)
		return nil
	}
	if rulesNode.Kind == yaml.AliasNode {
		return fmt.Errorf("top-level rules alias cannot be edited safely")
	}
	if rulesNode.Kind == yaml.ScalarNode && rulesNode.Tag == "!!null" && len(current) == 0 {
		replacement, err := encodeYAMLNode(nonNilRules(desired))
		if err != nil {
			return fmt.Errorf("encode rules: %w", err)
		}
		setTopLevelYAMLNode(root, "rules", replacement)
		return nil
	}
	if rulesNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("rules YAML node must be a sequence")
	}

	currentByID := make(map[string]engine.Rule, len(current))
	for _, rule := range current {
		currentByID[rule.RuleID] = rule
	}
	nodeByID := make(map[string]*yaml.Node, len(rulesNode.Content))
	for _, node := range rulesNode.Content {
		ruleID, err := yamlRuleID(node)
		if err != nil {
			return err
		}
		if _, duplicate := nodeByID[ruleID]; duplicate {
			return fmt.Errorf("duplicate rule id in YAML: %s", ruleID)
		}
		nodeByID[ruleID] = node
	}

	merged := make([]*yaml.Node, 0, len(desired))
	seen := make(map[string]struct{}, len(desired))
	for _, rule := range desired {
		ruleID := strings.TrimSpace(rule.RuleID)
		if _, duplicate := seen[ruleID]; duplicate {
			return fmt.Errorf("duplicate rule id: %s", ruleID)
		}
		seen[ruleID] = struct{}{}

		currentRule, exists := currentByID[ruleID]
		node, nodeExists := nodeByID[ruleID]
		if exists != nodeExists {
			return fmt.Errorf("rule %q does not match source YAML", ruleID)
		}
		if !exists {
			encoded, err := encodeYAMLNode(rule)
			if err != nil {
				return fmt.Errorf("encode rule %q: %w", ruleID, err)
			}
			merged = append(merged, encoded)
			continue
		}
		if node.Kind == yaml.AliasNode {
			if !reflect.DeepEqual(currentRule, rule) {
				return fmt.Errorf("rule %q is a YAML alias and cannot be edited safely", ruleID)
			}
			merged = append(merged, node)
			continue
		}
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("rule %q YAML node must be a mapping", ruleID)
		}
		if err := mergeRuleYAML(node, currentRule, rule); err != nil {
			return fmt.Errorf("rule %q: %w", ruleID, err)
		}
		merged = append(merged, node)
	}
	rulesNode.Content = merged
	return nil
}

func mergeRuleYAML(node *yaml.Node, current, desired engine.Rule) error {
	currentValue := reflect.ValueOf(current)
	desiredValue := reflect.ValueOf(desired)
	for _, field := range managedRuleYAMLFields {
		before := currentValue.Field(field.index)
		after := desiredValue.Field(field.index)
		if reflect.DeepEqual(before.Interface(), after.Interface()) {
			continue
		}
		if after.Kind() == reflect.Slice && after.IsNil() {
			after = reflect.MakeSlice(after.Type(), 0, 0)
		}
		replacement, err := encodeYAMLNode(after.Interface())
		if err != nil {
			return fmt.Errorf("encode %s: %w", field.name, err)
		}
		setYAMLMappingNode(node, field.name, replacement)
	}
	return nil
}

func yamlRuleID(node *yaml.Node) (string, error) {
	var identity struct {
		RuleID string `yaml:"rule_id"`
	}
	if err := node.Decode(&identity); err != nil {
		return "", fmt.Errorf("decode rule identity: %w", err)
	}
	ruleID := strings.TrimSpace(identity.RuleID)
	if ruleID == "" {
		return "", fmt.Errorf("rule YAML node is missing rule_id")
	}
	return ruleID, nil
}

func yamlMappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1], true
		}
	}
	return nil, false
}

func setYAMLMappingNode(mapping *yaml.Node, key string, replacement *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != key {
			continue
		}
		keyNode := mapping.Content[index]
		previous := mapping.Content[index+1]
		if keyNode.LineComment != "" && previous.LineComment == "" && replacement.LineComment == "" {
			replacement.LineComment = keyNode.LineComment
			keyNode.LineComment = ""
		}
		mapping.Content[index+1] = replaceYAMLNode(previous, replacement)
		return
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		replacement,
	)
}

func replaceYAMLNode(previous, replacement *yaml.Node) *yaml.Node {
	previousKind := previous.Kind
	headComment := previous.HeadComment
	lineComment := previous.LineComment
	footComment := previous.FootComment
	if replacement.HeadComment != "" {
		headComment = replacement.HeadComment
	}
	if replacement.LineComment != "" {
		lineComment = replacement.LineComment
	}
	if replacement.FootComment != "" {
		footComment = replacement.FootComment
	}
	style := previous.Style
	anchor := previous.Anchor

	if previousKind == yaml.AliasNode {
		replacement.HeadComment = headComment
		replacement.LineComment = lineComment
		replacement.FootComment = footComment
		return replacement
	}
	*previous = *replacement
	previous.HeadComment = headComment
	previous.LineComment = lineComment
	previous.FootComment = footComment
	if previous.Kind == previousKind {
		previous.Style = style
	}
	if (previous.Kind == yaml.SequenceNode || previous.Kind == yaml.MappingNode) && len(previous.Content) == 0 {
		previous.Style = yaml.FlowStyle
	}
	if previous.Anchor == "" {
		previous.Anchor = anchor
	}
	return previous
}

func parseYAMLDocument(raw []byte) (*yaml.Node, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root must be a YAML mapping")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple YAML documents are not supported")
		}
		return nil, err
	}
	return &document, nil
}

func encodeYAMLNode(value any) (*yaml.Node, error) {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	return &node, nil
}

func setTopLevelYAMLNode(root *yaml.Node, key string, replacement *yaml.Node) {
	setYAMLMappingNode(root, key, replacement)
}
