package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/dnsresolver"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
	"github.com/cloudapp3/vmflow/precheck"
	"gopkg.in/yaml.v3"
)

type initOptions struct {
	configPath     string
	protocol       string
	listenAddr     string
	listenPort     int
	targetAddr     string
	targetPort     int
	name           string
	yes            bool
	noAuth         bool
	start          bool
	installService bool
	interactive    bool
	translator     *i18n.Translator
	prepareService initServicePrepareFunc
	resolveTarget  initTargetResolveFunc
}

type initRunMode uint8

const (
	initRunModeNone initRunMode = iota
	initRunModeForeground
	initRunModeService
)

type initServicePrepareFunc func(string, *i18n.Translator) (service.InstallPlan, error)
type initTargetResolveFunc func(string) error

type initResult struct {
	ConfigPath     string
	ProfilePath    string
	ControlAddress string
	Rule           engine.Rule
	Language       string
	RunMode        initRunMode
	ServicePlan    service.InstallPlan
	configScope    appdirs.Scope
	setupUpdate    controlapi.SetupConfigUpdate
	preparedConfig config.File
	restoreProfile func() error
}

var errInitAborted = errors.New("initialization canceled")

func runInit(args []string) {
	runInitWithTranslator(args, defaultTranslator())
}

func runInitWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	options, err := parseInitOptionsWithTranslator(args, os.Stderr, translator)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.init.error.invalid_arguments", err))
		os.Exit(2)
	}
	options.interactive = isTerminalFile(os.Stdin) && isTerminalFile(os.Stdout)
	options.translator = translator
	result, err := executeInit(options, os.Stdin, os.Stdout)
	if result.Language != "" {
		translator = i18n.New(result.Language)
	}
	if errors.Is(err, errInitAborted) {
		fmt.Fprintln(os.Stdout, translator.T("cli.init.aborted"))
		return
	}
	if err != nil {
		err = restoreInitProfileAfterFailure(result, err)
		fmt.Fprintf(os.Stderr, "%s\n", translator.Tf("cli.init.error.failed", err))
		os.Exit(1)
	}
	switch result.RunMode {
	case initRunModeForeground:
		fmt.Fprintln(os.Stdout)
		runForegroundWithTranslator([]string{"-config", result.ConfigPath}, i18n.New(result.Language))
	case initRunModeService:
		fmt.Fprintln(os.Stdout)
		if err := runInitService(result, os.Stdout, translator); err != nil {
			err = restoreInitProfileAfterFailure(result, err)
			printInitServiceFailure(os.Stderr, result, translator, err)
			os.Exit(1)
		}
	}
}

func restoreInitProfileAfterFailure(result initResult, err error) error {
	if err == nil || result.restoreProfile == nil || initServiceConfigSaved(err) {
		return err
	}
	if restoreErr := result.restoreProfile(); restoreErr != nil {
		return fmt.Errorf("%w; restore local management profile: %v", err, restoreErr)
	}
	return err
}

func parseInitOptions(args []string, output io.Writer) (initOptions, error) {
	return parseInitOptionsWithTranslator(args, output, defaultTranslator())
}

func parseInitOptionsWithTranslator(args []string, output io.Writer, translator *i18n.Translator) (initOptions, error) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, translator.T("cli.init.usage"))
		fs.PrintDefaults()
	}
	options := initOptions{translator: translator}
	fs.StringVar(&options.configPath, "config", "", translator.T("cli.flag.config"))
	fs.StringVar(&options.protocol, "protocol", "", translator.T("cli.init.flag.protocol"))
	fs.StringVar(&options.listenAddr, "listen-address", "", translator.T("cli.init.flag.listen_address"))
	fs.IntVar(&options.listenPort, "listen-port", 0, translator.T("cli.init.flag.listen_port"))
	fs.StringVar(&options.targetAddr, "target-address", "", translator.T("cli.init.flag.target_address"))
	fs.IntVar(&options.targetPort, "target-port", 0, translator.T("cli.init.flag.target_port"))
	fs.StringVar(&options.name, "name", "", translator.T("cli.init.flag.name"))
	fs.BoolVar(&options.yes, "yes", false, translator.T("cli.init.flag.yes"))
	fs.BoolVar(&options.noAuth, "no-auth", false, translator.T("cli.init.flag.no_auth"))
	fs.BoolVar(&options.start, "start", false, translator.T("cli.init.flag.start"))
	fs.BoolVar(&options.installService, "install-service", false, translator.T("cli.init.flag.install_service"))
	if err := fs.Parse(args); err != nil {
		return initOptions{}, err
	}
	if options.start && options.installService {
		return initOptions{}, fmt.Errorf("%s", translator.T("cli.init.error.run_mode_conflict"))
	}
	if extra := fs.Args(); len(extra) != 0 {
		return initOptions{}, fmt.Errorf("%s", translator.Tf("cli.error.unexpected_arguments", extra))
	}
	options.configPath = strings.TrimSpace(options.configPath)
	if options.configPath != "" {
		absolute, err := filepath.Abs(options.configPath)
		if err != nil {
			return initOptions{}, fmt.Errorf("%s", translator.Tf("cli.init.error.resolve_config", err))
		}
		options.configPath = absolute
	}
	return options, nil
}

func executeInit(options initOptions, input io.Reader, output io.Writer) (initResult, error) {
	reader := bufio.NewReader(input)
	translator, language, err := selectInitLanguage(reader, output, translatorOrEnglish(options.translator), options.interactive)
	result := initResult{Language: language}
	if err != nil {
		return result, err
	}
	options.translator = translator
	runMode, servicePlan, configPath, configScope, err := resolveInitDestination(options, reader, output, translator)
	result = initResult{
		ConfigPath:  configPath,
		Language:    language,
		RunMode:     runMode,
		ServicePlan: servicePlan,
		configScope: configScope,
	}
	if err != nil {
		return result, err
	}
	options.configPath = configPath
	configDirectoryExisted, err := existingDirectory(filepath.Dir(options.configPath))
	if err != nil {
		return result, contextualizeInitConfigAccessError(result, err, translator)
	}
	cfg, configExists, err := loadSetupBase(options.configPath)
	if err != nil {
		return result, contextualizeInitConfigAccessError(result, err, translator)
	}
	targetResolver := options.resolveTarget
	if targetResolver == nil {
		configuredResolver := dnsresolver.New()
		targetResolver = func(host string) error {
			return resolveInitTargetHostnameWith(configuredResolver, host)
		}
	}

	address := controlAddressForConfig(cfg)
	probe := probeDaemon(address, loadManagementDefaults(io.Discard).Token, clientTLSOptionsFromEnv(), nil, 350*time.Millisecond)
	if probe.Status == "running" || probe.Status == "unknown" {
		return result, fmt.Errorf("%s", translator.Tf("cli.init.error.address_in_use", address, localizedState(translator, probe.Status)))
	}

	if configExists {
		fmt.Fprintf(output, "%s\n\n", translator.Tf("cli.init.config_existing", options.configPath, len(cfg.Rules)))
	} else {
		fmt.Fprintf(output, "%s\n\n", translator.Tf("cli.init.config_new", options.configPath))
	}

	if options.interactive && strings.TrimSpace(options.protocol) == "" {
		printInitProtocolGuidanceWithTranslator(output, translator)
	}
	protocol, err := collectInitProtocol(reader, output, options.protocol, options.interactive, translator)
	if err != nil {
		return result, err
	}
	if options.interactive && (strings.TrimSpace(options.listenAddr) == "" || options.listenPort == 0) {
		printInitListenerGuidanceWithTranslator(output, translator)
	}
	listenAddress, err := collectInitAddress(reader, output, translator.T("cli.init.label.listen_address"), options.listenAddr, config.DefaultControlHost, options.interactive, translator)
	if err != nil {
		return result, err
	}
	listenPort, err := collectPort(reader, output, translator.T("cli.init.label.listen_port"), options.listenPort, options.interactive, translator)
	if err != nil {
		return result, err
	}
	if options.interactive && (strings.TrimSpace(options.targetAddr) == "" || options.targetPort == 0) {
		printInitTargetGuidanceWithTranslator(output, translator)
	}
	targetAddress, err := collectInitTargetAddress(reader, output, options.targetAddr, "127.0.0.1", options.interactive, translator, targetResolver)
	if err != nil {
		return result, err
	}
	targetPort, err := collectPort(reader, output, translator.T("cli.init.label.target_port"), options.targetPort, options.interactive, translator)
	if err != nil {
		return result, err
	}
	defaultName := strings.ReplaceAll(protocol, "+", "-") + "-" + strconv.Itoa(listenPort)
	if options.interactive && strings.TrimSpace(options.name) == "" {
		printInitNameGuidanceWithTranslator(output, translator)
	}
	name, err := collectInitRuleName(reader, output, options.name, defaultName, options.interactive, translator)
	if err != nil {
		return result, err
	}

	if exposesNonLoopback(listenAddress) && !options.yes {
		if !options.interactive {
			return result, fmt.Errorf("%s", translator.Tf("cli.init.error.remote_traffic", listenAddress))
		}
		confirmed, err := promptConfirmRequired(reader, output, translator.Tf("cli.init.confirm.remote_traffic", listenAddress), translator)
		if err != nil {
			return result, err
		}
		if !confirmed {
			return result, errInitAborted
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
		return result, fmt.Errorf("%s", translator.Tf("cli.init.error.invalid_rule", err))
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
	desired.Language = language
	desired.Rules = rules
	adminToken := ""
	if !options.noAuth {
		var authErr error
		desired.Auth, adminToken, authErr = setupAuth(cfg.Auth)
		if authErr != nil {
			return result, authErr
		}
	}
	normalized, err := normalizeSetupConfig(desired)
	if err != nil {
		return result, err
	}
	if err := precheckSetupConfig(normalized, rule, options, reader, output); err != nil {
		return result, err
	}
	if adminToken != "" && configExists && runMode != initRunModeService {
		secured, err := secureConfigPermissions(options.configPath)
		if err != nil {
			return result, fmt.Errorf("secure config permissions before saving credentials: %w", err)
		}
		if secured {
			fmt.Fprintln(output, translator.T("cli.init.permissions_secured"))
		}
	}

	profilePath := ""
	var restoreProfile func() error
	if adminToken != "" {
		profilePath, err = clientconfig.DefaultPath()
		if err != nil {
			return result, fmt.Errorf("resolve client profile path: %w", err)
		}
		if sameFilesystemPath(profilePath, options.configPath) {
			return result, fmt.Errorf("client profile path must differ from config path")
		}
		profile := clientconfig.Profile{
			Address:    controlAddressForConfig(normalized),
			Token:      adminToken,
			ConfigPath: options.configPath,
		}
		restoreProfile, err = replaceClientProfile(profilePath, profile)
		if err != nil {
			return result, fmt.Errorf("save local management credentials: %w", err)
		}
	}

	setupUpdate := controlapi.SetupConfigUpdate{Language: normalized.Language, Auth: normalized.Auth, Rules: normalized.Rules}
	if replacedBundledExample {
		setupUpdate.RulesHeadComment = "Forwarding rules created by vmflow init or managed through the authenticated TUI."
	}
	result = initResult{
		ConfigPath:     options.configPath,
		ProfilePath:    profilePath,
		ControlAddress: controlAddressForConfig(normalized),
		Rule:           rule,
		Language:       normalized.Language,
		RunMode:        runMode,
		ServicePlan:    servicePlan,
		configScope:    configScope,
		setupUpdate:    setupUpdate,
		preparedConfig: normalized,
		restoreProfile: restoreProfile,
	}
	if runMode == initRunModeService {
		return result, nil
	}

	save, err := saveManagedSetupConfigWithClaim(options.configPath, appdirs.User, setupUpdate, !configDirectoryExisted)
	if err != nil {
		if restoreProfile != nil && !managedSetupConfigCommitted(err) {
			if restoreErr := restoreProfile(); restoreErr != nil {
				return result, fmt.Errorf("%w; restore previous client profile: %v", err, restoreErr)
			}
		}
		return result, err
	}
	result.Language = save.Config.Language
	result.ControlAddress = controlAddressForConfig(save.Config)
	result.restoreProfile = nil
	printInitConfigReady(output, result, save.Config, translator)
	if runMode == initRunModeNone {
		fmt.Fprintln(output, translator.Tf("cli.init.next_background", "vmflow init --install-service"))
		fmt.Fprintln(output, translator.Tf("cli.init.next_foreground", guideConfigCommand("vmflow run", statusReport{ConfigPath: options.configPath})))
	}
	return result, nil
}

func printInitConfigReady(output io.Writer, result initResult, saved config.File, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.T("cli.init.ready"))
	fmt.Fprintln(output, translator.Tf("cli.output.config_scope", translator.T("cli.output.config_scope.user")))
	fmt.Fprintln(output, translator.Tf("cli.output.config", result.ConfigPath))
	fmt.Fprintln(output, translator.Tf("cli.init.language_summary", saved.Language))
	fmt.Fprintln(output, translator.Tf("cli.init.rule_summary", result.Rule.RuleID, result.Rule.Protocol, result.Rule.ListenAddr, result.Rule.ListenPort, result.Rule.TargetAddr, result.Rule.TargetPort))
	if result.ProfilePath != "" {
		fmt.Fprintln(output, translator.Tf("cli.init.management_profile", result.ProfilePath))
	} else if !saved.Auth.Enabled {
		fmt.Fprintln(output, translator.T("cli.init.management_read_only"))
	}
}

func resolveInitDestination(options initOptions, reader *bufio.Reader, output io.Writer, translator *i18n.Translator) (initRunMode, service.InstallPlan, string, appdirs.Scope, error) {
	prepare := options.prepareService
	if prepare == nil {
		prepare = prepareInitServiceInstall
	}
	systemPath, err := resolveInitConfigPath(options.configPath, appdirs.System)
	if err != nil {
		return initRunModeNone, service.InstallPlan{}, "", appdirs.System, err
	}
	if options.installService {
		plan, err := prepare(systemPath, translator)
		return initRunModeService, plan, systemPath, appdirs.System, err
	}
	if options.start {
		userPath, err := resolveInitConfigPath(options.configPath, appdirs.User)
		return initRunModeForeground, service.InstallPlan{}, userPath, appdirs.User, err
	}
	if !options.interactive {
		userPath, err := resolveInitConfigPath(options.configPath, appdirs.User)
		return initRunModeNone, service.InstallPlan{}, userPath, appdirs.User, err
	}
	mode, plan, err := selectInitRunMode(reader, output, translator, systemPath, prepare)
	if err != nil {
		return mode, plan, systemPath, appdirs.System, err
	}
	if mode == initRunModeService {
		return mode, plan, systemPath, appdirs.System, nil
	}
	userPath, err := resolveInitConfigPath(options.configPath, appdirs.User)
	return mode, plan, userPath, appdirs.User, err
}

func resolveInitConfigPath(explicit string, scope appdirs.Scope) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit, nil
	}
	resolved, err := appdirs.ConfigPath(scope)
	if err != nil {
		return "", fmt.Errorf("resolve default config: %w", err)
	}
	return resolved, nil
}

func selectInitRunMode(reader *bufio.Reader, output io.Writer, translator *i18n.Translator, configPath string, prepare initServicePrepareFunc) (initRunMode, service.InstallPlan, error) {
	translator = translatorOrEnglish(translator)
	defaultChoice := "1"
	for {
		fmt.Fprintln(output, translator.T("cli.init.run_mode.title"))
		fmt.Fprintln(output, translator.T("cli.init.run_mode.option.service"))
		fmt.Fprintln(output, translator.T("cli.init.run_mode.option.service_detail"))
		fmt.Fprintln(output, translator.T("cli.init.run_mode.option.foreground"))
		fmt.Fprintln(output, translator.T("cli.init.run_mode.option.none"))
		choice, err := promptValue(reader, output, translator.T("cli.init.run_mode.choice"), defaultChoice, translator)
		if err != nil {
			return initRunModeNone, service.InstallPlan{}, err
		}
		switch strings.TrimSpace(choice) {
		case "1":
			plan, err := prepare(configPath, translator)
			if err != nil {
				fmt.Fprintln(output, translator.Tf("cli.init.service.unavailable", err))
				defaultChoice = "2"
				continue
			}
			printInitServicePlan(output, plan, translator)
			confirmed, err := promptConfirmRequired(reader, output, translator.T("cli.init.confirm.install_service"), translator)
			if err != nil {
				return initRunModeNone, service.InstallPlan{}, err
			}
			if confirmed {
				return initRunModeService, plan, nil
			}
			defaultChoice = "2"
		case "2":
			return initRunModeForeground, service.InstallPlan{}, nil
		case "3":
			return initRunModeNone, service.InstallPlan{}, nil
		default:
			fmt.Fprintln(output, translator.T("cli.init.run_mode.invalid"))
		}
	}
}

func selectInitLanguage(reader *bufio.Reader, output io.Writer, translator *i18n.Translator, interactive bool) (*i18n.Translator, string, error) {
	translator = translatorOrEnglish(translator)
	if !interactive {
		return translator, translator.Locale(), nil
	}
	fmt.Fprintln(output, translator.T("cli.init.label.language"))
	fmt.Fprintln(output, translator.T("cli.init.language_option.en"))
	fmt.Fprintln(output, translator.T("cli.init.language_option.zh_cn"))
	defaultChoice := "1"
	if translator.Locale() == i18n.SimplifiedChinese {
		defaultChoice = "2"
	}
	for {
		value, err := promptValue(reader, output, translator.T("cli.init.label.language_choice"), defaultChoice, translator)
		if err != nil {
			return nil, "", err
		}
		switch strings.TrimSpace(value) {
		case "1":
			return i18n.New(i18n.English), i18n.English, nil
		case "2":
			return i18n.New(i18n.SimplifiedChinese), i18n.SimplifiedChinese, nil
		}
		language, ok := i18n.Match(value)
		if ok {
			return i18n.New(language), language, nil
		}
		fmt.Fprintln(output, translator.T("cli.init.prompt.language"))
	}
}

func printInitProtocolGuidance(output io.Writer) {
	printInitProtocolGuidanceWithTranslator(output, defaultTranslator())
}

func printInitProtocolGuidanceWithTranslator(output io.Writer, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.T("cli.init.guidance.protocol.title"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.protocol.tcp"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.protocol.udp"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.protocol.both"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.accept_default"))
	fmt.Fprintln(output)
}

func printInitListenerGuidance(output io.Writer) {
	printInitListenerGuidanceWithTranslator(output, defaultTranslator())
}

func printInitListenerGuidanceWithTranslator(output io.Writer, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.T("cli.init.guidance.listener.title"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.listener.description"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.listener.loopback"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.listener.public"))
	fmt.Fprintln(output)
}

func printInitTargetGuidance(output io.Writer) {
	printInitTargetGuidanceWithTranslator(output, defaultTranslator())
}

func printInitTargetGuidanceWithTranslator(output io.Writer, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.T("cli.init.guidance.target.title"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.target.description"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.target.address"))
	fmt.Fprintln(output)
}

func printInitNameGuidance(output io.Writer) {
	printInitNameGuidanceWithTranslator(output, defaultTranslator())
}

func printInitNameGuidanceWithTranslator(output io.Writer, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(output, translator.T("cli.init.guidance.name.title"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.name.description"))
	fmt.Fprintln(output, translator.T("cli.init.guidance.name.default"))
	fmt.Fprintln(output)
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
		Language:       i18n.English,
		ControlPort:    config.DefaultControlPort,
		UDPMaxSessions: engine.DefaultUDPGlobalMaxSessions,
		Log: config.LogConfig{
			Level:  config.DefaultLogLevel,
			Format: config.DefaultLogFormat,
		},
		Rules: []engine.Rule{},
	}, false, nil
}

func contextualizeInitConfigAccessError(result initResult, err error, translator *i18n.Translator) error {
	if result.RunMode != initRunModeService || !errors.Is(err, os.ErrPermission) {
		return err
	}
	translator = translatorOrEnglish(translator)
	message := translator.Tf("cli.init.error.system_config_permission", result.ConfigPath)
	return fmt.Errorf("%s: %w", message, err)
}

func sameFilesystemPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

type initValueNormalizer func(string) (string, error)

func collectValidatedInitValue(reader *bufio.Reader, output io.Writer, label, supplied, defaultValue string, interactive bool, translator *i18n.Translator, normalize initValueNormalizer) (string, error) {
	suppliedValue := strings.TrimSpace(supplied) != ""
	for {
		value, err := collectValue(reader, output, label, supplied, defaultValue, interactive, translator)
		if err != nil {
			return "", err
		}
		normalized, validationErr := normalize(value)
		if validationErr == nil {
			return normalized, nil
		}
		if !interactive || suppliedValue {
			return "", validationErr
		}
		fmt.Fprintln(output, validationErr)
		supplied = ""
	}
}

func collectInitProtocol(reader *bufio.Reader, output io.Writer, supplied string, interactive bool, translator *i18n.Translator) (string, error) {
	translator = translatorOrEnglish(translator)
	return collectValidatedInitValue(reader, output, translator.T("cli.init.label.protocol"), supplied, "tcp", interactive, translator, func(value string) (string, error) {
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case string(engine.ProtocolTCP), string(engine.ProtocolUDP), string(engine.ProtocolTCPUDP):
			return value, nil
		default:
			return "", fmt.Errorf("%s", translator.T("cli.init.error.protocol"))
		}
	})
}

func collectInitAddress(reader *bufio.Reader, output io.Writer, label, supplied, defaultValue string, interactive bool, translator *i18n.Translator) (string, error) {
	translator = translatorOrEnglish(translator)
	return collectValidatedInitValue(reader, output, label, supplied, defaultValue, interactive, translator, func(value string) (string, error) {
		value = strings.TrimSpace(value)
		if _, err := netip.ParseAddr(value); err == nil || validInitHostname(value) {
			return value, nil
		}
		return "", fmt.Errorf("%s", translator.Tf("cli.init.prompt.address_format", label))
	})
}

func collectInitTargetAddress(reader *bufio.Reader, output io.Writer, supplied, defaultValue string, interactive bool, translator *i18n.Translator, resolve initTargetResolveFunc) (string, error) {
	translator = translatorOrEnglish(translator)
	label := translator.T("cli.init.label.target_address")
	if resolve == nil {
		resolve = resolveInitTargetHostname
	}
	suppliedValue := strings.TrimSpace(supplied) != ""
	for {
		address, err := collectInitAddress(reader, output, label, supplied, defaultValue, interactive, translator)
		if err != nil {
			return "", err
		}
		if _, err := netip.ParseAddr(address); err == nil || resolve(address) == nil {
			return address, nil
		}
		resolveErr := fmt.Errorf("%s", translator.Tf("cli.init.prompt.target_unresolvable", address))
		if !interactive || suppliedValue {
			return "", resolveErr
		}
		fmt.Fprintln(output, resolveErr)
		supplied = ""
	}
}

func resolveInitTargetHostname(host string) error {
	return resolveInitTargetHostnameWith(dnsresolver.New(), host)
}

func resolveInitTargetHostnameWith(resolver *dnsresolver.Resolver, host string) error {
	addresses, err := resolver.LookupHost(context.Background(), host)
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return fmt.Errorf("hostname resolved without an address")
	}
	return nil
}

func collectInitRuleName(reader *bufio.Reader, output io.Writer, supplied, defaultValue string, interactive bool, translator *i18n.Translator) (string, error) {
	translator = translatorOrEnglish(translator)
	label := translator.T("cli.init.label.rule_name")
	return collectValidatedInitValue(reader, output, label, supplied, defaultValue, interactive, translator, func(value string) (string, error) {
		value = strings.TrimSpace(value)
		if value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 64 && !hasUnsupportedPromptCharacters(value) {
			return value, nil
		}
		return "", fmt.Errorf("%s", translator.T("cli.init.prompt.rule_name_format"))
	})
}

func validInitHostname(value string) bool {
	if value == "" || len(value) > 254 {
		return false
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	numericAddress := strings.Contains(value, ".")
	for index := 0; index < len(value); index++ {
		if value[index] != '.' && (value[index] < '0' || value[index] > '9') {
			numericAddress = false
			break
		}
	}
	if numericAddress {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
				continue
			}
			return false
		}
	}
	return true
}

func cleanPromptInput(line string) (value string, strippedTerminalControl, valid bool) {
	cleaned := ansi.Strip(line)
	strippedTerminalControl = cleaned != line
	cleaned = strings.TrimSuffix(cleaned, "\n")
	cleaned = strings.TrimSuffix(cleaned, "\r")
	if hasUnsupportedPromptCharacters(cleaned) {
		return "", strippedTerminalControl, false
	}
	return strings.TrimSpace(cleaned), strippedTerminalControl, true
}

func hasUnsupportedPromptCharacters(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, character := range value {
		if !unicode.IsPrint(character) {
			return true
		}
	}
	return false
}

func collectValue(reader *bufio.Reader, output io.Writer, label, supplied, defaultValue string, interactive bool, translator *i18n.Translator) (string, error) {
	translator = translatorOrEnglish(translator)
	if value := strings.TrimSpace(supplied); value != "" {
		return value, nil
	}
	if !interactive {
		if defaultValue != "" {
			return defaultValue, nil
		}
		return "", fmt.Errorf("%s", translator.Tf("cli.init.error.required_noninteractive", label))
	}
	return promptValue(reader, output, label, defaultValue, translator)
}

func collectPort(reader *bufio.Reader, output io.Writer, label string, supplied int, interactive bool, translator *i18n.Translator) (int, error) {
	translator = translatorOrEnglish(translator)
	if supplied != 0 {
		if supplied < 1 || supplied > 65535 {
			return 0, fmt.Errorf("%s", translator.Tf("cli.init.error.port_range", label))
		}
		return supplied, nil
	}
	if !interactive {
		return 0, fmt.Errorf("%s", translator.Tf("cli.init.error.required_noninteractive", label))
	}
	for {
		value, err := promptValue(reader, output, label, "", translator)
		if err != nil {
			return 0, err
		}
		port, err := strconv.Atoi(value)
		if err == nil && port >= 1 && port <= 65535 {
			return port, nil
		}
		fmt.Fprintln(output, translator.T("cli.init.prompt.port_range"))
	}
}

func promptValue(reader *bufio.Reader, output io.Writer, label, defaultValue string, translator *i18n.Translator) (string, error) {
	translator = translatorOrEnglish(translator)
	if defaultValue == "" {
		fmt.Fprintf(output, "%s: ", label)
	} else {
		fmt.Fprintf(output, "%s [%s]: ", label, defaultValue)
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value, strippedTerminalControl, valid := cleanPromptInput(line)
	if !valid {
		fmt.Fprintln(output, translator.T("cli.init.prompt.unsupported_characters"))
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s", translator.Tf("cli.init.error.input_ended_value", label))
		}
		return promptValue(reader, output, label, defaultValue, translator)
	}
	if strippedTerminalControl {
		fmt.Fprintln(output, translator.T("cli.init.prompt.terminal_control_ignored"))
		if value != "" {
			fmt.Fprintln(output, translator.Tf("cli.init.prompt.recognized_value", label, value))
		}
	}
	if value == "" {
		value = defaultValue
	}
	if value == "" {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s", translator.Tf("cli.init.error.input_ended_value", label))
		}
		return promptValue(reader, output, label, defaultValue, translator)
	}
	return value, nil
}

func promptConfirm(reader *bufio.Reader, output io.Writer, label string, defaultYes bool, translator *i18n.Translator) (bool, error) {
	defaultChoice := confirmationDefaultNo
	if defaultYes {
		defaultChoice = confirmationDefaultYes
	}
	return promptConfirmChoice(reader, output, label, defaultChoice, translator)
}

type confirmationDefault uint8

const (
	confirmationRequired confirmationDefault = iota
	confirmationDefaultNo
	confirmationDefaultYes
)

func promptConfirmRequired(reader *bufio.Reader, output io.Writer, label string, translator *i18n.Translator) (bool, error) {
	return promptConfirmChoice(reader, output, label, confirmationRequired, translator)
}

func promptConfirmChoice(reader *bufio.Reader, output io.Writer, label string, defaultChoice confirmationDefault, translator *i18n.Translator) (bool, error) {
	translator = translatorOrEnglish(translator)
	suffix := translator.T("cli.init.prompt.confirm_required")
	switch defaultChoice {
	case confirmationDefaultNo:
		suffix = translator.T("cli.init.prompt.confirm_no")
	case confirmationDefaultYes:
		suffix = translator.T("cli.init.prompt.confirm_yes")
	}
	for {
		fmt.Fprintf(output, "%s? %s ", label, suffix)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		answer, strippedTerminalControl, valid := cleanPromptInput(line)
		if !valid {
			fmt.Fprintln(output, translator.T("cli.init.prompt.unsupported_characters"))
			if errors.Is(err, io.EOF) {
				return false, fmt.Errorf("%s", translator.T("cli.init.error.input_ended_confirmation"))
			}
			continue
		}
		if strippedTerminalControl {
			fmt.Fprintln(output, translator.T("cli.init.prompt.terminal_control_ignored"))
		}
		answer = strings.ToLower(answer)
		switch answer {
		case "":
			switch defaultChoice {
			case confirmationDefaultNo:
				return false, nil
			case confirmationDefaultYes:
				return true, nil
			}
			fmt.Fprintln(output, translator.T("cli.init.prompt.yes_no"))
		case "y", "yes", "是", "好":
			return true, nil
		case "n", "no", "否", "不":
			return false, nil
		default:
			fmt.Fprintln(output, translator.T("cli.init.prompt.yes_no"))
		}
		if errors.Is(err, io.EOF) {
			return false, fmt.Errorf("%s", translator.T("cli.init.error.input_ended_confirmation"))
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
	translator := translatorOrEnglish(options.translator)
	full := precheck.CheckConfig(cfg, nil, precheck.Options{})
	if !full.OK {
		printPrecheckFindingsWithTranslator(output, full, translator)
		return fmt.Errorf("%s", translator.Tf("cli.init.error.config_precheck", full.ErrorCount))
	}
	newRuleConfig := cfg
	newRuleConfig.Rules = []engine.Rule{newRule}
	result := precheck.CheckConfig(newRuleConfig, nil, precheck.DefaultOptions())
	if !result.OK {
		printPrecheckFindingsWithTranslator(output, result, translator)
		return fmt.Errorf("%s", translator.Tf("cli.init.error.rule_precheck", result.ErrorCount))
	}
	warnings := append([]precheck.Item(nil), full.Items...)
	warnings = append(warnings, result.Items...)
	warnings = uniqueWarnings(warnings)
	if len(warnings) == 0 {
		return nil
	}
	printPrecheckItemsWithTranslator(output, warnings, translator)
	if options.yes {
		return nil
	}
	if !options.interactive {
		return fmt.Errorf("%s", translator.T("cli.init.error.precheck_warnings"))
	}
	confirmed, err := promptConfirmRequired(reader, output, translator.T("cli.init.confirm.warnings"), translator)
	if err != nil {
		return err
	}
	if !confirmed {
		return errInitAborted
	}
	return nil
}

func printPrecheckFindings(output io.Writer, result precheck.Result) {
	printPrecheckFindingsWithTranslator(output, result, defaultTranslator())
}

func printPrecheckItems(output io.Writer, items []precheck.Item) {
	printPrecheckItemsWithTranslator(output, items, defaultTranslator())
}

func printPrecheckFindingsWithTranslator(output io.Writer, result precheck.Result, translator *i18n.Translator) {
	printPrecheckItemsWithTranslator(output, result.Items, translator)
}

func printPrecheckItemsWithTranslator(output io.Writer, items []precheck.Item, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	for _, item := range items {
		severity := strings.ToUpper(string(item.Severity))
		switch item.Severity {
		case precheck.SeverityWarning:
			severity = translator.T("severity.warning")
		case precheck.SeverityError:
			severity = translator.T("severity.error")
		}
		fmt.Fprintf(output, "%s: %s", severity, item.Message)
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
