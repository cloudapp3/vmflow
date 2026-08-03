package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
)

func TestExecuteInitLocalizesProtocolValidation(t *testing.T) {
	options := initOptions{
		configPath:  filepath.Join(t.TempDir(), "config.yaml"),
		protocol:    "invalid",
		interactive: false,
		translator:  i18n.New(i18n.SimplifiedChinese),
	}
	_, err := executeInit(options, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "协议必须是") {
		t.Fatalf("Chinese protocol validation error = %v", err)
	}
}

func TestExecuteInitPreservesServiceDestinationOnValidationFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	plan := service.InstallPlan{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: configPath, ServiceName: "vmflow"}
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "invalid", installService: true,
		translator: i18n.New(i18n.SimplifiedChinese),
		prepareService: func(string, *i18n.Translator) (service.InstallPlan, error) {
			return plan, nil
		},
	}, strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("invalid protocol was accepted")
	}
	if result.RunMode != initRunModeService || result.ConfigPath != configPath || result.ServicePlan != plan || result.Language != i18n.SimplifiedChinese {
		t.Fatalf("failure result lost destination context: %+v", result)
	}
}

func TestContextualizeInitConfigAccessErrorExplainsSystemPrivileges(t *testing.T) {
	wantErr := fmt.Errorf("open config: %w", os.ErrPermission)
	result := initResult{RunMode: initRunModeService, ConfigPath: "/etc/vmflow/config.yaml"}
	err := contextualizeInitConfigAccessError(result, wantErr, i18n.New(i18n.SimplifiedChinese))
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "管理员权限") || !strings.Contains(err.Error(), result.ConfigPath) {
		t.Fatalf("system permission error = %v", err)
	}
	if got := contextualizeInitConfigAccessError(initResult{RunMode: initRunModeForeground}, wantErr, i18n.New(i18n.English)); got != wantErr {
		t.Fatalf("foreground permission error was rewritten: %v", got)
	}
}

func TestExecuteInitCreatesRuleAuthAndClientProfile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	profilePath := filepath.Join(dir, "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", profilePath)
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")
	port := availableTCPPort(t)

	var output bytes.Buffer
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: port,
		targetAddr: "127.0.0.1", targetPort: 22, name: "Local SSH", yes: true,
	}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("executeInit: %v\n%s", err, output.String())
	}
	if result.RunMode != initRunModeNone || result.Rule.RuleID != "local-ssh" || result.ProfilePath != profilePath {
		t.Fatalf("result = %+v", result)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Language != i18n.English || !cfg.Auth.Enabled || len(cfg.Auth.Tokens) != 1 || len(cfg.Rules) != 1 || !cfg.Rules[0].Enabled {
		t.Fatalf("config = %+v", cfg)
	}
	profile, err := clientconfig.Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Token != cfg.Auth.Tokens[0].Token || profile.ConfigPath != configPath {
		t.Fatalf("profile/config mismatch: %+v / %+v", profile, cfg.Auth)
	}
	if strings.Contains(output.String(), profile.Token) {
		t.Fatal("management token leaked to output")
	}
	for _, unexpected := range []string{"Forwarding protocol", "Incoming listener", "Forward destination", "Rule name\n"} {
		if strings.Contains(output.String(), unexpected) {
			t.Fatalf("non-interactive init contains interactive guidance %q:\n%s", unexpected, output.String())
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %v, err = %v", info.Mode(), err)
		}
	}
}

func TestExecuteInitMarksCanonicalConfigWhenClientProfileCreatesDirectoryFirst(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	configPath, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(filepath.Dir(configPath), "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", profilePath)
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")

	_, err = executeInit(initOptions{
		protocol: "tcp", listenAddr: "127.0.0.1", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "managed", yes: true,
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientconfig.Load(profilePath); err != nil {
		t.Fatalf("load client profile: %v", err)
	}
	raw, err := os.ReadFile(appdirs.ManagedMarkerPath(configPath))
	if err != nil || string(raw) != "vmflow\n" {
		t.Fatalf("managed marker = %q, %v", raw, err)
	}
}

func TestExecuteInitExplainsInteractivePrompts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")

	var output bytes.Buffer
	result, err := executeInit(initOptions{
		configPath:  configPath,
		listenPort:  availableTCPPort(t),
		targetPort:  22,
		yes:         true,
		start:       true,
		interactive: true,
	}, strings.NewReader("\n\n\n\n\n"), &output)
	if err != nil {
		t.Fatalf("executeInit: %v\n%s", err, output.String())
	}
	if result.RunMode != initRunModeForeground || result.Rule.Protocol != "tcp" || result.Rule.ListenAddr != "127.0.0.1" || result.Rule.TargetAddr != "127.0.0.1" {
		t.Fatalf("result = %+v", result)
	}

	text := output.String()
	for _, want := range []string{
		"Language / 语言",
		"1) English (en)",
		"2) 简体中文 (zh-CN)",
		"Choose / 请选择 [1]:",
		"Forwarding protocol",
		"tcp: SSH, HTTP, databases, and most services.",
		"Press Enter to accept a value shown in [brackets].",
		"Incoming listener",
		"127.0.0.1 accepts connections from this server only.",
		"A server IP or 0.0.0.0 can accept remote traffic; review firewall rules first.",
		"Forward destination",
		"Use 127.0.0.1 when it runs on this server; otherwise enter its host or IP.",
		"This label appears in status, TUI, and logs.",
		"Protocol [tcp]:",
		"Listen address [127.0.0.1]:",
		"Target address [127.0.0.1]:",
		"Language: en",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("interactive init output does not contain %q:\n%s", want, text)
		}
	}
}

func TestExecuteInitExplainsInteractivePromptsInChinese(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")

	var output bytes.Buffer
	result, err := executeInit(initOptions{
		configPath:  configPath,
		listenPort:  availableTCPPort(t),
		targetPort:  22,
		yes:         true,
		start:       true,
		interactive: true,
	}, strings.NewReader("2\n\n\n\n\n"), &output)
	if err != nil {
		t.Fatalf("executeInit: %v\n%s", err, output.String())
	}
	if result.RunMode != initRunModeForeground {
		t.Fatalf("result = %+v", result)
	}

	text := output.String()
	for _, want := range []string{
		"Language / 语言",
		"1) English (en)",
		"2) 简体中文 (zh-CN)",
		"Choose / 请选择 [1]:",
		"转发协议",
		"tcp：适用于 SSH、HTTP、数据库和大多数服务",
		"传入监听",
		"127.0.0.1 仅接受来自本机的连接",
		"转发目标",
		"协议 [tcp]:",
		"监听地址 [127.0.0.1]:",
		"目标地址 [127.0.0.1]:",
		"配置已就绪",
		"语言：zh-CN",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Chinese init output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "cli.") || strings.Contains(text, "Forwarding protocol") {
		t.Fatalf("Chinese init leaked a key or English guidance:\n%s", text)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Language != i18n.SimplifiedChinese {
		t.Fatalf("saved language = %q", cfg.Language)
	}
}

func TestExecuteInitSelectsLanguageBeforeRunMode(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")

	var output bytes.Buffer
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "language-first", yes: true, interactive: true,
	}, strings.NewReader("2\n3\n"), &output)
	if err != nil {
		t.Fatalf("executeInit: %v\n%s", err, output.String())
	}
	if result.Language != i18n.SimplifiedChinese || result.RunMode != initRunModeNone {
		t.Fatalf("result = %+v", result)
	}

	text := output.String()
	languageIndex := strings.Index(text, "Language / 语言")
	runModeIndex := strings.Index(text, "选择 vmflow 运行方式")
	if languageIndex < 0 || runModeIndex < 0 || languageIndex >= runModeIndex {
		t.Fatalf("language selection must precede run mode selection:\n%s", text)
	}
	if strings.Contains(text, "Choose how to run vmflow") {
		t.Fatalf("run mode did not use the selected language:\n%s", text)
	}
}

func TestSelectInitLanguageOffersNumberedChoices(t *testing.T) {
	tests := []struct {
		name       string
		initial    string
		input      string
		want       string
		wantPrompt string
	}{
		{name: "default English", initial: i18n.English, input: "\n", want: i18n.English, wantPrompt: "Choose / 请选择 [1]:"},
		{name: "choose Chinese", initial: i18n.English, input: "2\n", want: i18n.SimplifiedChinese, wantPrompt: "Choose / 请选择 [1]:"},
		{name: "preserve Chinese default", initial: i18n.SimplifiedChinese, input: "\n", want: i18n.SimplifiedChinese, wantPrompt: "Choose / 请选择 [2]:"},
		{name: "legacy locale input", initial: i18n.English, input: "zh-CN\n", want: i18n.SimplifiedChinese, wantPrompt: "Choose / 请选择 [1]:"},
		{name: "retry invalid choice", initial: i18n.English, input: "3\n2\n", want: i18n.SimplifiedChinese, wantPrompt: "Enter 1 or 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			translator, language, err := selectInitLanguage(
				bufio.NewReader(strings.NewReader(test.input)),
				&output,
				i18n.New(test.initial),
				true,
			)
			if err != nil {
				t.Fatalf("selectInitLanguage: %v", err)
			}
			if language != test.want || translator.Locale() != test.want {
				t.Fatalf("language = %q, translator = %q; want %q", language, translator.Locale(), test.want)
			}
			text := output.String()
			for _, want := range []string{"1) English (en)", "2) 简体中文 (zh-CN)", test.wantPrompt} {
				if !strings.Contains(text, want) {
					t.Fatalf("language choices omitted %q:\n%s", want, text)
				}
			}
		})
	}
}

func TestInteractivePromptsStripTerminalControlSequences(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "focus events", input: "\x1b[O\x1b[I43.239.249.178\n", want: "43.239.249.178"},
		{name: "bracketed paste", input: "\x1b[200~43.239.249.178\x1b[201~\n", want: "43.239.249.178"},
		{name: "ordinary Unicode", input: "东京 转发\n", want: "东京 转发"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := promptValue(
				bufio.NewReader(strings.NewReader(test.input)),
				io.Discard,
				"Target address",
				"",
				i18n.New(i18n.English),
			)
			if err != nil {
				t.Fatalf("promptValue: %v", err)
			}
			if got != test.want {
				t.Fatalf("promptValue = %q, want %q", got, test.want)
			}
		})
	}

	confirmed, err := promptConfirmRequired(
		bufio.NewReader(strings.NewReader("\x1b[O\x1b[Iy\n")),
		io.Discard,
		"Continue",
		i18n.New(i18n.English),
	)
	if err != nil || !confirmed {
		t.Fatalf("promptConfirmRequired = %t, %v; want true, nil", confirmed, err)
	}

	var output bytes.Buffer
	got, err := promptValue(
		bufio.NewReader(strings.NewReader("\x1b[O\x1b[I43.239.249.178\n")),
		&output,
		"目标地址",
		"",
		i18n.New(i18n.SimplifiedChinese),
	)
	if err != nil || got != "43.239.249.178" {
		t.Fatalf("promptValue = %q, %v", got, err)
	}
	text := output.String()
	if !strings.Contains(text, "检测到终端控制字符，已自动忽略") || !strings.Contains(text, "识别到目标地址：43.239.249.178") {
		t.Fatalf("terminal-control guidance missing:\n%s", text)
	}
	if strings.Contains(text, "\x1b") {
		t.Fatalf("terminal control sequence leaked to output: %q", text)
	}
}

func TestCollectInitFieldsRetryRecoverableInteractiveInput(t *testing.T) {
	translator := i18n.New(i18n.SimplifiedChinese)

	var addressOutput bytes.Buffer
	address, err := collectInitAddress(
		bufio.NewReader(strings.NewReader("https://43.239.249.178:39887/path\n43.239.249.178\n")),
		&addressOutput,
		translator.T("cli.init.label.target_address"),
		"",
		"127.0.0.1",
		true,
		translator,
	)
	if err != nil || address != "43.239.249.178" {
		t.Fatalf("collectInitAddress = %q, %v", address, err)
	}
	if !strings.Contains(addressOutput.String(), "目标地址格式无效") || strings.Count(addressOutput.String(), "目标地址 [127.0.0.1]:") != 2 {
		t.Fatalf("address retry guidance missing:\n%s", addressOutput.String())
	}

	var protocolOutput bytes.Buffer
	protocol, err := collectInitProtocol(
		bufio.NewReader(strings.NewReader("ftp\ntcp+udp\n")),
		&protocolOutput,
		"",
		true,
		translator,
	)
	if err != nil || protocol != "tcp+udp" {
		t.Fatalf("collectInitProtocol = %q, %v", protocol, err)
	}
	if !strings.Contains(protocolOutput.String(), "协议必须是 tcp、udp 或 tcp+udp") {
		t.Fatalf("protocol retry guidance missing:\n%s", protocolOutput.String())
	}

	var nameOutput bytes.Buffer
	name, err := collectInitRuleName(
		bufio.NewReader(strings.NewReader("bad\x00name\n"+strings.Repeat("x", 65)+"\njp22-v2\n")),
		&nameOutput,
		"",
		"tcp-39999",
		true,
		translator,
	)
	if err != nil || name != "jp22-v2" {
		t.Fatalf("collectInitRuleName = %q, %v", name, err)
	}
	if !strings.Contains(nameOutput.String(), "输入包含不支持的控制字符") || !strings.Contains(nameOutput.String(), "规则名称必须包含 1 到 64 个可见字符") {
		t.Fatalf("rule-name retry guidance missing:\n%s", nameOutput.String())
	}
}

func TestCollectInitAddressValidatesInteractiveAndSuppliedValues(t *testing.T) {
	translator := i18n.New(i18n.English)
	for _, value := range []string{"43.239.249.178", "2001:db8::1", "backend.example.com", "service_name", "backend.example.com."} {
		got, err := collectInitAddress(bufio.NewReader(strings.NewReader("")), io.Discard, "Target address", value, "", false, translator)
		if err != nil || got != value {
			t.Fatalf("collectInitAddress(%q) = %q, %v", value, got, err)
		}
	}

	for _, value := range []string{"https://backend.example.com", "backend.example.com:443", "[2001:db8::1]", "999.999.999.999", "bad host"} {
		if _, err := collectInitAddress(bufio.NewReader(strings.NewReader("")), io.Discard, "Target address", value, "", false, translator); err == nil || !strings.Contains(err.Error(), "Target address is invalid") {
			t.Fatalf("collectInitAddress(%q) error = %v", value, err)
		}
	}
}

func TestCollectInitTargetAddressRetriesUnresolvableHostname(t *testing.T) {
	translator := i18n.New(i18n.SimplifiedChinese)
	var resolved []string
	resolve := func(host string) error {
		resolved = append(resolved, host)
		if host == "adfad" {
			return errors.New("no such host")
		}
		return nil
	}

	var output bytes.Buffer
	address, err := collectInitTargetAddress(
		bufio.NewReader(strings.NewReader("adfad\nbackend.internal\n")),
		&output,
		"",
		"127.0.0.1",
		true,
		translator,
		resolve,
	)
	if err != nil || address != "backend.internal" {
		t.Fatalf("collectInitTargetAddress = %q, %v", address, err)
	}
	if !reflect.DeepEqual(resolved, []string{"adfad", "backend.internal"}) {
		t.Fatalf("resolved hosts = %v", resolved)
	}
	text := output.String()
	if !strings.Contains(text, `无法通过 DNS 解析目标主机 "adfad"`) || strings.Count(text, "目标地址 [127.0.0.1]:") != 2 {
		t.Fatalf("target resolution retry guidance missing:\n%s", text)
	}

	resolveCalls := 0
	address, err = collectInitTargetAddress(
		bufio.NewReader(strings.NewReader("")),
		io.Discard,
		"43.239.249.178",
		"",
		false,
		i18n.New(i18n.English),
		func(string) error {
			resolveCalls++
			return errors.New("should not resolve an IP")
		},
	)
	if err != nil || address != "43.239.249.178" || resolveCalls != 0 {
		t.Fatalf("IP target = %q, calls = %d, err = %v", address, resolveCalls, err)
	}

	_, err = collectInitTargetAddress(
		bufio.NewReader(strings.NewReader("")),
		io.Discard,
		"adfad",
		"",
		false,
		i18n.New(i18n.English),
		func(string) error { return errors.New("no such host") },
	)
	if err == nil || !strings.Contains(err.Error(), `Target host "adfad" could not be resolved by DNS`) {
		t.Fatalf("non-interactive target resolution error = %v", err)
	}
}

func TestParseInitOptionsRejectsConflictingRunModes(t *testing.T) {
	_, err := parseInitOptionsWithTranslator(
		[]string{"-start", "-install-service"},
		io.Discard,
		i18n.New(i18n.SimplifiedChinese),
	)
	if err == nil || !strings.Contains(err.Error(), "不能同时使用") {
		t.Fatalf("run mode conflict error = %v", err)
	}
}

func TestResolveInitDestinationUsesScopeDefaultsAndExplicitOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", root)
	userPath, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}

	mode, _, gotPath, scope, err := resolveInitDestination(
		initOptions{start: true}, bufio.NewReader(strings.NewReader("")), io.Discard, i18n.New(i18n.English),
	)
	if err != nil || mode != initRunModeForeground || scope != appdirs.User || gotPath != userPath {
		t.Fatalf("foreground destination = mode %v, scope %v, path %q, err %v", mode, scope, gotPath, err)
	}

	systemPath, err := appdirs.SystemConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	wantPlan := service.InstallPlan{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: systemPath, ServiceName: "vmflow"}
	mode, plan, gotPath, scope, err := resolveInitDestination(initOptions{
		installService: true,
		prepareService: func(path string, _ *i18n.Translator) (service.InstallPlan, error) {
			if path != systemPath {
				t.Fatalf("system prepare path = %q", path)
			}
			return wantPlan, nil
		},
	}, bufio.NewReader(strings.NewReader("")), io.Discard, i18n.New(i18n.English))
	if err != nil || mode != initRunModeService || scope != appdirs.System || gotPath != systemPath || plan != wantPlan {
		t.Fatalf("system destination = mode %v, scope %v, path %q, plan %+v, err %v", mode, scope, gotPath, plan, err)
	}

	explicit := filepath.Join(root, "custom.yaml")
	_, _, gotPath, scope, err = resolveInitDestination(
		initOptions{configPath: explicit, start: true}, bufio.NewReader(strings.NewReader("")), io.Discard, i18n.New(i18n.English),
	)
	if err != nil || scope != appdirs.User || gotPath != explicit {
		t.Fatalf("explicit destination = scope %v, path %q, err %v", scope, gotPath, err)
	}
}

func TestSelectInitRunModeRetriesAndReturnsToMenu(t *testing.T) {
	plan := service.InstallPlan{
		BinaryPath:  "/usr/local/bin/vmflow",
		ConfigPath:  "/etc/vmflow/config.yaml",
		ServiceName: "vmflow",
	}
	prepareCalls := 0
	prepare := func(configPath string, translator *i18n.Translator) (service.InstallPlan, error) {
		prepareCalls++
		if configPath != plan.ConfigPath || translator.Locale() != i18n.SimplifiedChinese {
			t.Fatalf("prepare args = %q, %q", configPath, translator.Locale())
		}
		return plan, nil
	}
	var output bytes.Buffer
	mode, gotPlan, err := selectInitRunMode(
		bufio.NewReader(strings.NewReader("9\n1\nn\n\n")),
		&output,
		i18n.New(i18n.SimplifiedChinese),
		plan.ConfigPath,
		prepare,
	)
	if err != nil {
		t.Fatalf("selectInitRunMode: %v", err)
	}
	if mode != initRunModeForeground || gotPlan != (service.InstallPlan{}) {
		t.Fatalf("mode = %v, plan = %+v", mode, gotPlan)
	}
	if prepareCalls != 1 {
		t.Fatalf("prepare calls = %d", prepareCalls)
	}
	text := output.String()
	for _, want := range []string{
		"安装系统服务并在后台运行",
		"请输入 1、2 或 3",
		"系统服务安装计划",
		"程序：/usr/local/bin/vmflow",
		"安装并启动该系统服务? [y/n]",
		"请选择 vmflow 运行方式 [2]:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run mode output missing %q:\n%s", want, text)
		}
	}
}

func TestSelectInitRunModeDefaultsToConfirmedService(t *testing.T) {
	wantPlan := service.InstallPlan{
		BinaryPath:  "/usr/local/bin/vmflow",
		ConfigPath:  "/etc/vmflow/config.yaml",
		ServiceName: "vmflow",
	}
	var output bytes.Buffer
	mode, plan, err := selectInitRunMode(
		bufio.NewReader(strings.NewReader("\ny\n")),
		&output,
		i18n.New(i18n.English),
		wantPlan.ConfigPath,
		func(string, *i18n.Translator) (service.InstallPlan, error) { return wantPlan, nil },
	)
	if err != nil {
		t.Fatalf("selectInitRunMode: %v", err)
	}
	if mode != initRunModeService || plan != wantPlan {
		t.Fatalf("mode = %v, plan = %+v", mode, plan)
	}
}

func TestSelectInitRunModeFallsBackAfterUnavailableService(t *testing.T) {
	var output bytes.Buffer
	mode, _, err := selectInitRunMode(
		bufio.NewReader(strings.NewReader("1\n\n")),
		&output,
		i18n.New(i18n.English),
		"/tmp/config.yaml",
		func(string, *i18n.Translator) (service.InstallPlan, error) {
			return service.InstallPlan{}, errors.New("binary is under a user-writable directory")
		},
	)
	if err != nil {
		t.Fatalf("selectInitRunMode: %v", err)
	}
	if mode != initRunModeForeground {
		t.Fatalf("mode = %v", mode)
	}
	text := output.String()
	if !strings.Contains(text, "System service installation is unavailable") || !strings.Contains(text, "Choose how to run vmflow [2]:") {
		t.Fatalf("unavailable service output:\n%s", text)
	}
}

func TestExecuteInitSupportsNonInteractiveServiceInstall(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	profilePath := filepath.Join(dir, "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", profilePath)
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")
	wantPlan := service.InstallPlan{
		BinaryPath:  "/usr/local/bin/vmflow",
		ConfigPath:  configPath,
		ServiceName: "vmflow",
	}
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "service", yes: true, installService: true,
		prepareService: func(path string, translator *i18n.Translator) (service.InstallPlan, error) {
			if path != configPath || translator.Locale() != i18n.English {
				t.Fatalf("prepare args = %q, %q", path, translator.Locale())
			}
			return wantPlan, nil
		},
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("executeInit: %v", err)
	}
	if result.RunMode != initRunModeService || result.ServicePlan != wantPlan {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecuteInitDoesNotWriteConfigWhenServicePreparationFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	wantErr := errors.New("binary is under a user-writable directory")
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "service", yes: true, installService: true,
		prepareService: func(string, *i18n.Translator) (service.InstallPlan, error) {
			return service.InstallPlan{}, wantErr
		},
	}, strings.NewReader(""), io.Discard)
	if !errors.Is(err, wantErr) {
		t.Fatalf("executeInit error = %v", err)
	}
	if result.RunMode != initRunModeService || result.ConfigPath != configPath {
		t.Fatalf("result = %+v", result)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config should not be written before service preparation succeeds: %v", statErr)
	}
}

func TestPromptConfirmRequiredRetriesUntilExplicitAnswer(t *testing.T) {
	var output bytes.Buffer
	confirmed, err := promptConfirmRequired(
		bufio.NewReader(strings.NewReader("maybe\n\ny\n")),
		&output,
		"0.0.0.0 可能接收远程流量，是否继续",
		i18n.New(i18n.SimplifiedChinese),
	)
	if err != nil {
		t.Fatalf("promptConfirmRequired: %v", err)
	}
	if !confirmed {
		t.Fatal("explicit y was not accepted")
	}
	text := output.String()
	if got := strings.Count(text, "[y/n]"); got != 3 {
		t.Fatalf("confirmation prompt count = %d, want 3:\n%s", got, text)
	}
	if got := strings.Count(text, "请输入 y/yes/是 或 n/no/否。"); got != 2 {
		t.Fatalf("invalid-answer guidance count = %d, want 2:\n%s", got, text)
	}
}

func TestExecuteInitExposureDeclineReturnsCleanCancellation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	var output bytes.Buffer
	result, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "0.0.0.0", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "public", interactive: true,
	}, strings.NewReader("2\n2\nn\n"), &output)
	if !errors.Is(err, errInitAborted) {
		t.Fatalf("executeInit error = %v, want cancellation", err)
	}
	if result.Language != i18n.SimplifiedChinese {
		t.Fatalf("cancellation language = %q", result.Language)
	}
	if !strings.Contains(output.String(), "[y/n]") {
		t.Fatalf("explicit confirmation prompt missing:\n%s", output.String())
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config should not be written after cancellation: %v", statErr)
	}
}

func TestExecuteInitReplacesUntouchedBundledExample(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	port := availableTCPPort(t)
	_, err = executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: port,
		targetAddr: "127.0.0.1", targetPort: 22, name: "replacement", yes: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].RuleID != "replacement" {
		t.Fatalf("bundled example was not replaced: %+v", cfg.Rules)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "bundled example is loopback-only and disabled") || !strings.Contains(string(saved), "Forwarding rules created by vmflow init") {
		t.Fatalf("bundled example comment was not replaced:\n%s", saved)
	}
}

func TestExecuteInitSecuresWorldReadableConfigBeforeAddingToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nauth:\n  enabled: false\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VMFLOW_CLIENT_CONFIG", filepath.Join(dir, "client.yaml"))
	_, err := executeInit(initOptions{
		configPath: configPath, protocol: "tcp", listenAddr: "127.0.0.1", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "secure", yes: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSecureConfigPermissionsPreservesGroupReadableConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nrules: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := secureConfigPermissions(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("0640 service config should not be changed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %o, want 640", info.Mode().Perm())
	}
}

func TestSecureConfigPermissionsRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink semantics")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := secureConfigPermissions(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode = %o, want 644", info.Mode().Perm())
	}
}

func TestExecuteInitRequiresExposureConfirmation(t *testing.T) {
	dir := t.TempDir()
	options := initOptions{
		configPath: filepath.Join(dir, "config.yaml"), protocol: "tcp", listenAddr: "0.0.0.0", listenPort: availableTCPPort(t),
		targetAddr: "127.0.0.1", targetPort: 22, name: "public",
	}
	_, err := executeInit(options, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "remote traffic") {
		t.Fatalf("exposure error = %v", err)
	}
	if _, statErr := os.Stat(options.configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config should not be written: %v", statErr)
	}
}

func TestExecuteInitNoAuthLeavesTUIReadOnly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	profilePath := filepath.Join(dir, "client.yaml")
	t.Setenv("VMFLOW_CLIENT_CONFIG", profilePath)
	_, err := executeInit(initOptions{
		configPath: configPath, protocol: "udp", listenAddr: "127.0.0.1", listenPort: availableUDPPort(t),
		targetAddr: "1.1.1.1", targetPort: 53, name: "dns", yes: true, noAuth: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Enabled {
		t.Fatal("auth unexpectedly enabled")
	}
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("client profile unexpectedly written: %v", err)
	}
}

func TestRuleIDAndPlaceholderHelpers(t *testing.T) {
	if got := slugRuleID("  SSH / Tokyo 01 "); got != "ssh-tokyo-01" {
		t.Fatalf("slugRuleID = %q", got)
	}
	if !isPlaceholderToken("change-me") || isPlaceholderToken("real-secret-value") {
		t.Fatal("placeholder classification is wrong")
	}
}

func TestReplaceClientProfileRestoresPreviousState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	oldProfile := clientconfig.Profile{Address: "http://127.0.0.1:19090", Token: "old-token"}
	if err := clientconfig.Save(path, oldProfile); err != nil {
		t.Fatal(err)
	}
	restore, err := replaceClientProfile(path, clientconfig.Profile{Address: "http://127.0.0.1:19123", Token: "new-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	got, err := clientconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != oldProfile.Address || got.Token != oldProfile.Token {
		t.Fatalf("restored profile = %+v", got)
	}

	newPath := filepath.Join(t.TempDir(), "new-client.yaml")
	restore, err = replaceClientProfile(newPath, clientconfig.Profile{Address: "http://127.0.0.1:19090", Token: "temporary"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("new profile was not removed: %v", err)
	}
}

func availableTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func availableUDPPort(t *testing.T) int {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	port := connection.LocalAddr().(*net.UDPAddr).Port
	_ = connection.Close()
	return port
}
