package controlapi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/engine"
	"gopkg.in/yaml.v3"
)

// SetupConfigUpdate is the configuration surface owned by the first-run
// workflow. Existing files retain every other top-level field and unknown YAML
// node.
type SetupConfigUpdate struct {
	Auth             config.AuthConfig
	Rules            []engine.Rule
	RulesHeadComment string
}

// SetupConfigCommitted reports whether SaveSetupConfig reached its filesystem
// commit point before returning an error (for example, a later directory-sync
// failure). Callers must not roll back companion state in that case.
func SetupConfigCommitted(err error) bool {
	var commitErr *configCommitError
	return errors.As(err, &commitErr) && commitErr.Committed
}

// SaveSetupConfig validates and persists the auth/rules chosen by the first-run
// workflow. Existing files use the same guarded atomic replacement as live TUI
// rule management. A missing file is created exclusively with mode 0600.
func SaveSetupConfig(path string, update SetupConfigUpdate) (config.File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return config.File{}, fmt.Errorf("save setup config: missing config path")
	}
	if !filepath.IsAbs(path) {
		return config.File{}, fmt.Errorf("save setup config: config path must be absolute")
	}

	document, err := loadConfigDocument(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return createSetupConfig(path, update)
		}
		return config.File{}, fmt.Errorf("save setup config: %w", err)
	}

	firstRaw, err := renderSetupConfig(document.raw, update.Auth, update.Rules, update.RulesHeadComment)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: render candidate: %w", err)
	}
	firstConfig, err := config.Parse(firstRaw)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: validate candidate: %w", err)
	}
	raw, err := renderSetupConfig(document.raw, firstConfig.Auth, firstConfig.Rules, update.RulesHeadComment)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: render normalized candidate: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: validate normalized candidate: %w", err)
	}
	candidate := &configCandidate{
		Config:         cfg,
		Revision:       configRevision(raw),
		path:           path,
		raw:            raw,
		sourceRevision: document.Revision,
	}
	staged, err := candidate.Stage()
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: %w", err)
	}
	defer staged.Discard()
	if err := staged.Commit(); err != nil {
		return cfg, fmt.Errorf("save setup config: %w", err)
	}
	return cfg, nil
}

type initialSetupDocument struct {
	Version        int               `yaml:"version"`
	ControlPort    int               `yaml:"control_port"`
	UDPMaxSessions int               `yaml:"udp_max_sessions"`
	Log            config.LogConfig  `yaml:"log"`
	Auth           config.AuthConfig `yaml:"auth"`
	Rules          []engine.Rule     `yaml:"rules"`
}

func createSetupConfig(path string, update SetupConfigUpdate) (config.File, error) {
	document := initialSetupDocument{
		Version:        1,
		ControlPort:    config.DefaultControlPort,
		UDPMaxSessions: engine.DefaultUDPGlobalMaxSessions,
		Log: config.LogConfig{
			Level:  config.DefaultLogLevel,
			Format: config.DefaultLogFormat,
		},
		Auth:  update.Auth,
		Rules: append([]engine.Rule(nil), update.Rules...),
	}
	if document.Rules == nil {
		document.Rules = []engine.Rule{}
	}
	firstRaw, err := yaml.Marshal(document)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: encode new config: %w", err)
	}
	firstConfig, err := config.Parse(firstRaw)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: validate new config: %w", err)
	}
	document.Auth = firstConfig.Auth
	document.Rules = firstConfig.Rules
	document.UDPMaxSessions = firstConfig.UDPMaxSessions
	raw, err := yaml.Marshal(document)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: encode normalized config: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: validate normalized config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return config.File{}, fmt.Errorf("save setup config: create config directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return config.File{}, fmt.Errorf("save setup config: create config: %w", err)
	}
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		return config.File{}, fmt.Errorf("save setup config: write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return config.File{}, fmt.Errorf("save setup config: sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return config.File{}, fmt.Errorf("save setup config: close config: %w", err)
	}
	removeOnError = false
	if err := syncConfigDirectory(dir); err != nil {
		return cfg, &configCommitError{
			Committed: true,
			Err:       fmt.Errorf("save setup config: config created but parent directory sync failed: %w", err),
		}
	}
	return cfg, nil
}

func renderSetupConfig(source []byte, auth config.AuthConfig, rules []engine.Rule, rulesHeadComment string) ([]byte, error) {
	document, err := parseYAMLDocument(source)
	if err != nil {
		return nil, err
	}
	root := document.Content[0]
	if rules == nil {
		rules = []engine.Rule{}
	}
	authNode, err := encodeYAMLNode(auth)
	if err != nil {
		return nil, fmt.Errorf("encode auth: %w", err)
	}
	rulesNode, err := encodeYAMLNode(rules)
	if err != nil {
		return nil, fmt.Errorf("encode rules: %w", err)
	}
	setTopLevelYAMLNode(root, "auth", authNode)
	setTopLevelYAMLNode(root, "rules", rulesNode)
	if strings.TrimSpace(rulesHeadComment) != "" {
		setTopLevelYAMLHeadComment(root, "rules", strings.TrimSpace(rulesHeadComment))
	}
	return encodeYAMLDocument(document)
}

func setTopLevelYAMLHeadComment(root *yaml.Node, key, comment string) {
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == key {
			root.Content[index].HeadComment = comment
			return
		}
	}
}

func encodeYAMLDocument(document *yaml.Node) ([]byte, error) {
	var output strings.Builder
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return []byte(output.String()), nil
}
