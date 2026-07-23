package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/precheck"
	"gopkg.in/yaml.v3"
)

type initOptions struct {
	configPath  string
	protocol    string
	listenAddr  string
	listenPort  int
	targetAddr  string
	targetPort  int
	name        string
	yes         bool
	noAuth      bool
	start       bool
	interactive bool
}

type initResult struct {
	ConfigPath  string
	ProfilePath string
	Rule        engine.Rule
	Start       bool
}

func runInit(args []string) {
	options, err := parseInitOptions(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid init arguments: %v\n", err)
		os.Exit(2)
	}
	options.interactive = isTerminalFile(os.Stdin) && isTerminalFile(os.Stdout)
	result, err := executeInit(options, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmflow init failed: %v\n", err)
		os.Exit(1)
	}
	if result.Start {
		fmt.Fprintln(os.Stdout)
		runForeground([]string{"-config", result.ConfigPath})
	}
}

func parseInitOptions(args []string, output io.Writer) (initOptions, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage:\n  vmflow init [flags]\n\nCreates and validates the first forwarding rule. Missing values are prompted on a terminal.\n\nOptions:")
		fs.PrintDefaults()
	}
	var options initOptions
	fs.StringVar(&options.configPath, "config", "", "config file path (default: config.yaml beside vmflow)")
	fs.StringVar(&options.protocol, "protocol", "", "forwarding protocol: tcp, udp, or tcp+udp")
	fs.StringVar(&options.listenAddr, "listen-address", "", "listen address (safe default: 127.0.0.1)")
	fs.IntVar(&options.listenPort, "listen-port", 0, "listen port")
	fs.StringVar(&options.targetAddr, "target-address", "", "target host or IP (default: 127.0.0.1)")
	fs.IntVar(&options.targetPort, "target-port", 0, "target port")
	fs.StringVar(&options.name, "name", "", "rule name")
	fs.BoolVar(&options.yes, "yes", false, "accept exposure and precheck warning confirmations")
	fs.BoolVar(&options.noAuth, "no-auth", false, "do not enable management authentication automatically (TUI editing may remain read-only)")
	fs.BoolVar(&options.start, "start", false, "start vmflow in the foreground after saving")
	if err := fs.Parse(args); err != nil {
		return initOptions{}, err
	}
	if extra := fs.Args(); len(extra) != 0 {
		return initOptions{}, fmt.Errorf("unexpected argument(s): %v", extra)
	}
	options.configPath = strings.TrimSpace(options.configPath)
	if options.configPath == "" {
		resolved, err := defaultRuntimeConfigPath()
		if err != nil {
			return initOptions{}, fmt.Errorf("resolve default config path: %w", err)
		}
		options.configPath = resolved
	}
	absolute, err := filepath.Abs(options.configPath)
	if err != nil {
		return initOptions{}, fmt.Errorf("resolve config path: %w", err)
	}
	options.configPath = absolute
	return options, nil
}

func executeInit(options initOptions, input io.Reader, output io.Writer) (initResult, error) {
	reader := bufio.NewReader(input)
	cfg, configExists, err := loadSetupBase(options.configPath)
	if err != nil {
		return initResult{}, err
	}

	address := controlAddressForConfig(cfg)
	probe := probeDaemon(address, loadManagementDefaults(io.Discard).Token, clientTLSOptionsFromEnv(), nil, 350*time.Millisecond)
	if probe.Status == "running" || probe.Status == "unknown" {
		return initResult{}, fmt.Errorf("a process is already using %s (%s); stop the running daemon before changing first-run configuration", address, probe.Status)
	}

	if configExists {
		fmt.Fprintf(output, "Config: %s (%d existing rule(s))\n\n", options.configPath, len(cfg.Rules))
	} else {
		fmt.Fprintf(output, "Config: %s (new)\n\n", options.configPath)
	}

	protocol, err := collectValue(reader, output, "Protocol", options.protocol, "tcp", options.interactive)
	if err != nil {
		return initResult{}, err
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol != string(engine.ProtocolTCP) && protocol != string(engine.ProtocolUDP) && protocol != string(engine.ProtocolTCPUDP) {
		return initResult{}, fmt.Errorf("protocol must be tcp, udp, or tcp+udp")
	}
	listenAddress, err := collectValue(reader, output, "Listen address", options.listenAddr, config.DefaultControlHost, options.interactive)
	if err != nil {
		return initResult{}, err
	}
	listenPort, err := collectPort(reader, output, "Listen port", options.listenPort, options.interactive)
	if err != nil {
		return initResult{}, err
	}
	targetAddress, err := collectValue(reader, output, "Target address", options.targetAddr, "127.0.0.1", options.interactive)
	if err != nil {
		return initResult{}, err
	}
	targetPort, err := collectPort(reader, output, "Target port", options.targetPort, options.interactive)
	if err != nil {
		return initResult{}, err
	}
	defaultName := strings.ReplaceAll(protocol, "+", "-") + "-" + strconv.Itoa(listenPort)
	name, err := collectValue(reader, output, "Rule name", options.name, defaultName, options.interactive)
	if err != nil {
		return initResult{}, err
	}

	if exposesNonLoopback(listenAddress) && !options.yes {
		if !options.interactive {
			return initResult{}, fmt.Errorf("listen address %s may accept remote traffic; rerun with -yes after reviewing firewall exposure", listenAddress)
		}
		confirmed, err := promptConfirm(reader, output, fmt.Sprintf("%s may accept remote traffic. Continue", listenAddress), false)
		if err != nil {
			return initResult{}, err
		}
		if !confirmed {
			return initResult{}, fmt.Errorf("aborted")
		}
	}

	now := time.Now().Unix()
	rule := engine.Rule{
		Name:        strings.TrimSpace(name),
		Protocol:    engine.Protocol(protocol),
		ListenAddr:  strings.TrimSpace(listenAddress),
		ListenPort:  listenPort,
		TargetAddr:  strings.TrimSpace(targetAddress),
		TargetPort:  targetPort,
		Enabled:     true,
		IdleTimeout: 300,
		CreatedTime: now,
		UpdatedTime: now,
	}
	rule.RuleID = uniqueRuleID(slugRuleID(rule.Name), cfg.Rules)
	if err := rule.Validate(); err != nil {
		return initResult{}, fmt.Errorf("invalid forwarding rule: %w", err)
	}

	rules := append([]engine.Rule(nil), cfg.Rules...)
	replacedBundledExample := false
	if len(rules) == 1 && isBundledExampleRule(rules[0]) {
		rules[0] = rule
		replacedBundledExample = true
	} else {
		rules = append(rules, rule)
	}
	desired := cfg
	desired.Rules = rules
	adminToken := ""
	if !options.noAuth {
		var authErr error
		desired.Auth, adminToken, authErr = setupAuth(cfg.Auth)
		if authErr != nil {
			return initResult{}, authErr
		}
	}
	normalized, err := normalizeSetupConfig(desired)
	if err != nil {
		return initResult{}, err
	}
	if err := precheckSetupConfig(normalized, rule, options, reader, output); err != nil {
		return initResult{}, err
	}
	if adminToken != "" && configExists {
		secured, err := secureConfigPermissions(options.configPath)
		if err != nil {
			return initResult{}, fmt.Errorf("secure config permissions before saving credentials: %w", err)
		}
		if secured {
			fmt.Fprintln(output, "Secured config permissions: 0600")
		}
	}

	profilePath := ""
	var restoreProfile func() error
	if adminToken != "" {
		profilePath, err = clientconfig.DefaultPath()
		if err != nil {
			return initResult{}, fmt.Errorf("resolve client profile path: %w", err)
		}
		if sameFilesystemPath(profilePath, options.configPath) {
			return initResult{}, fmt.Errorf("client profile path must differ from config path")
		}
		profile := clientconfig.Profile{
			Address:    controlAddressForConfig(normalized),
			Token:      adminToken,
			ConfigPath: options.configPath,
		}
		restoreProfile, err = replaceClientProfile(profilePath, profile)
		if err != nil {
			return initResult{}, fmt.Errorf("save local management credentials: %w", err)
		}
	}

	setupUpdate := controlapi.SetupConfigUpdate{Auth: normalized.Auth, Rules: normalized.Rules}
	if replacedBundledExample {
		setupUpdate.RulesHeadComment = "Forwarding rules created by vmflow init or managed through the authenticated TUI."
	}
	saved, err := controlapi.SaveSetupConfig(options.configPath, setupUpdate)
	if err != nil {
		if restoreProfile != nil && !controlapi.SetupConfigCommitted(err) {
			if restoreErr := restoreProfile(); restoreErr != nil {
				return initResult{}, fmt.Errorf("%w; restore previous client profile: %v", err, restoreErr)
			}
		}
		return initResult{}, err
	}

	fmt.Fprintln(output, "Configuration ready")
	fmt.Fprintf(output, "Config: %s\n", options.configPath)
	fmt.Fprintf(output, "Rule: %s (%s %s:%d -> %s:%d)\n", rule.RuleID, rule.Protocol, rule.ListenAddr, rule.ListenPort, rule.TargetAddr, rule.TargetPort)
	if profilePath != "" {
		fmt.Fprintf(output, "Management: authenticated local profile saved to %s\n", profilePath)
	} else if !saved.Auth.Enabled {
		fmt.Fprintln(output, "Management: read-only until authentication is configured")
	}

	start := options.start
	if options.interactive && !start {
		start, err = promptConfirm(reader, output, "Start vmflow now", true)
		if err != nil {
			return initResult{}, err
		}
	}
	if !start {
		fmt.Fprintln(output, "\nNext: vmflow run")
	}
	return initResult{ConfigPath: options.configPath, ProfilePath: profilePath, Rule: rule, Start: start}, nil
}

func secureConfigPermissions(path string) (bool, error) {
	if runtime.GOOS == "windows" {
		return false, nil
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return false, fmt.Errorf("config path must be a regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return false, fmt.Errorf("config file changed while securing permissions")
	}
	// Group-read (0640) is allowed for a dedicated service account. Group
	// write/execute and every other-user permission are unsafe for bearer tokens.
	if openedInfo.Mode().Perm()&0o037 == 0 {
		return false, nil
	}
	if err := file.Chmod(0o600); err != nil {
		return false, err
	}
	return true, nil
}

func replaceClientProfile(path string, replacement clientconfig.Profile) (func() error, error) {
	previous, err := clientconfig.Load(path)
	previousExists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load existing client profile: %w", err)
	}
	if err := clientconfig.Save(path, replacement); err != nil {
		return nil, err
	}
	return func() error {
		if previousExists {
			return clientconfig.Save(path, previous)
		}
		current, err := clientconfig.Load(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Address != replacement.Address || current.Token != replacement.Token || current.ConfigPath != replacement.ConfigPath {
			return fmt.Errorf("client profile changed after setup write")
		}
		return os.Remove(path)
	}, nil
}

func loadSetupBase(path string) (config.File, bool, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return config.File{}, false, fmt.Errorf("load config %s: config path must not be a symlink", path)
	} else if err != nil && !os.IsNotExist(err) {
		return config.File{}, false, fmt.Errorf("inspect config %s: %w", path, err)
	}
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return config.File{}, false, fmt.Errorf("load config %s: %w", path, err)
	}
	return config.File{
		Version:        1,
		ControlPort:    config.DefaultControlPort,
		UDPMaxSessions: engine.DefaultUDPGlobalMaxSessions,
		Log: config.LogConfig{
			Level:  config.DefaultLogLevel,
			Format: config.DefaultLogFormat,
		},
		Rules: []engine.Rule{},
	}, false, nil
}

func sameFilesystemPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func collectValue(reader *bufio.Reader, output io.Writer, label, supplied, defaultValue string, interactive bool) (string, error) {
	if value := strings.TrimSpace(supplied); value != "" {
		return value, nil
	}
	if !interactive {
		if defaultValue != "" {
			return defaultValue, nil
		}
		return "", fmt.Errorf("%s is required in non-interactive mode", strings.ToLower(label))
	}
	return promptValue(reader, output, label, defaultValue)
}

func collectPort(reader *bufio.Reader, output io.Writer, label string, supplied int, interactive bool) (int, error) {
	if supplied != 0 {
		if supplied < 1 || supplied > 65535 {
			return 0, fmt.Errorf("%s must be between 1 and 65535", strings.ToLower(label))
		}
		return supplied, nil
	}
	if !interactive {
		return 0, fmt.Errorf("%s is required in non-interactive mode", strings.ToLower(label))
	}
	for {
		value, err := promptValue(reader, output, label, "")
		if err != nil {
			return 0, err
		}
		port, err := strconv.Atoi(value)
		if err == nil && port >= 1 && port <= 65535 {
			return port, nil
		}
		fmt.Fprintln(output, "Enter a port between 1 and 65535.")
	}
}

func promptValue(reader *bufio.Reader, output io.Writer, label, defaultValue string) (string, error) {
	if defaultValue == "" {
		fmt.Fprintf(output, "%s: ", label)
	} else {
		fmt.Fprintf(output, "%s [%s]: ", label, defaultValue)
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		value = defaultValue
	}
	if value == "" {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("input ended before %s was provided", strings.ToLower(label))
		}
		return promptValue(reader, output, label, defaultValue)
	}
	return value, nil
}

func promptConfirm(reader *bufio.Reader, output io.Writer, label string, defaultYes bool) (bool, error) {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	for {
		fmt.Fprintf(output, "%s? %s ", label, suffix)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		switch answer {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(output, "Enter y or n.")
		}
		if errors.Is(err, io.EOF) {
			return false, fmt.Errorf("input ended before confirmation")
		}
	}
}

func setupAuth(current config.AuthConfig) (config.AuthConfig, string, error) {
	tokens := make([]config.AuthToken, 0, len(current.Tokens)+1)
	adminToken := ""
	for _, item := range current.Tokens {
		item.Token = strings.TrimSpace(item.Token)
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role == "" {
			role = config.AuthRoleAdmin
		}
		if item.Token == "" || isPlaceholderToken(item.Token) {
			continue
		}
		item.Role = role
		tokens = append(tokens, item)
		if adminToken == "" && role == config.AuthRoleAdmin {
			adminToken = item.Token
		}
	}
	if adminToken == "" {
		generated, err := generateAdminToken()
		if err != nil {
			return config.AuthConfig{}, "", fmt.Errorf("generate management token: %w", err)
		}
		adminToken = generated
		tokens = append(tokens, config.AuthToken{Name: "local-admin", Token: generated, Role: config.AuthRoleAdmin})
	}
	return config.AuthConfig{Enabled: true, Tokens: tokens}, adminToken, nil
}

func generateAdminToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func isPlaceholderToken(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "change-me", "changeme", "replace-me":
		return true
	default:
		return false
	}
}

func normalizeSetupConfig(cfg config.File) (config.File, error) {
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return config.File{}, fmt.Errorf("encode setup configuration: %w", err)
	}
	normalized, err := config.Parse(raw)
	if err != nil {
		return config.File{}, fmt.Errorf("validate setup configuration: %w", err)
	}
	return normalized, nil
}

func precheckSetupConfig(cfg config.File, newRule engine.Rule, options initOptions, reader *bufio.Reader, output io.Writer) error {
	full := precheck.CheckConfig(cfg, nil, precheck.Options{})
	if !full.OK {
		printPrecheckFindings(output, full)
		return fmt.Errorf("configuration precheck failed with %d error(s)", full.ErrorCount)
	}
	newRuleConfig := cfg
	newRuleConfig.Rules = []engine.Rule{newRule}
	result := precheck.CheckConfig(newRuleConfig, nil, precheck.DefaultOptions())
	if !result.OK {
		printPrecheckFindings(output, result)
		return fmt.Errorf("new rule precheck failed with %d error(s)", result.ErrorCount)
	}
	warnings := append([]precheck.Item(nil), full.Items...)
	warnings = append(warnings, result.Items...)
	warnings = uniqueWarnings(warnings)
	if len(warnings) == 0 {
		return nil
	}
	printPrecheckItems(output, warnings)
	if options.yes {
		return nil
	}
	if !options.interactive {
		return fmt.Errorf("precheck returned warnings; rerun with -yes after reviewing them")
	}
	confirmed, err := promptConfirm(reader, output, "Continue despite these warnings", false)
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("aborted")
	}
	return nil
}

func printPrecheckFindings(output io.Writer, result precheck.Result) {
	printPrecheckItems(output, result.Items)
}

func printPrecheckItems(output io.Writer, items []precheck.Item) {
	for _, item := range items {
		fmt.Fprintf(output, "%s: %s", strings.ToUpper(string(item.Severity)), item.Message)
		if item.RuleID != "" {
			fmt.Fprintf(output, " [%s]", item.RuleID)
		}
		fmt.Fprintln(output)
	}
}

func uniqueWarnings(items []precheck.Item) []precheck.Item {
	seen := make(map[string]struct{}, len(items))
	result := make([]precheck.Item, 0, len(items))
	for _, item := range items {
		if item.Severity != precheck.SeverityWarning {
			continue
		}
		key := string(item.Severity) + "\x00" + item.Check + "\x00" + item.RuleID + "\x00" + item.Message
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func slugRuleID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var result strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			result.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func uniqueRuleID(base string, existing []engine.Rule) string {
	if base == "" {
		base = "forward"
	}
	seen := make(map[string]struct{}, len(existing))
	for _, rule := range existing {
		seen[rule.RuleID] = struct{}{}
	}
	if _, ok := seen[base]; !ok {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if _, ok := seen[candidate]; !ok {
			return candidate
		}
	}
}

func isBundledExampleRule(rule engine.Rule) bool {
	rule = rule.Standardize()
	return rule.RuleID == "ssh-forward" && rule.Name == "ssh-forward" &&
		rule.Protocol == engine.ProtocolTCP && rule.ListenAddr == "127.0.0.1" && rule.ListenPort == 2201 &&
		rule.TargetAddr == "127.0.0.1" && rule.TargetPort == 22 && !rule.Enabled &&
		rule.SpeedLimit == 0 && rule.MaxConn == 0 && rule.IdleTimeout == 300 &&
		(rule.SourceIPMode == "" || rule.SourceIPMode == engine.SourceIPModeOff) && len(rule.SourceIPs) == 0 && rule.Remark == "example"
}

func exposesNonLoopback(address string) bool {
	address = strings.TrimSpace(address)
	if strings.EqualFold(address, "localhost") {
		return false
	}
	ip := net.ParseIP(address)
	return ip == nil || !ip.IsLoopback()
}

func isTerminalFile(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
