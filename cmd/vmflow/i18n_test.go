package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/i18n"
)

func TestExtractLanguageArgsSupportsGlobalAndCommandPositions(t *testing.T) {
	tests := []struct {
		input    []string
		wantLang string
		wantArgs []string
	}{
		{input: []string{"--lang", "zh-CN", "init", "-yes"}, wantLang: "zh-CN", wantArgs: []string{"init", "-yes"}},
		{input: []string{"init", "--lang=zh_CN.UTF-8", "-yes"}, wantLang: "zh_CN.UTF-8", wantArgs: []string{"init", "-yes"}},
		{input: []string{"status", "-lang=en"}, wantLang: "en", wantArgs: []string{"status"}},
		{input: []string{"run", "--", "--lang", "zh-CN"}, wantArgs: []string{"run", "--", "--lang", "zh-CN"}},
	}
	for _, test := range tests {
		gotLang, gotArgs, err := extractLanguageArgs(test.input)
		if err != nil {
			t.Fatalf("extractLanguageArgs(%v): %v", test.input, err)
		}
		if gotLang != test.wantLang || !reflect.DeepEqual(gotArgs, test.wantArgs) {
			t.Fatalf("extractLanguageArgs(%v) = %q, %v; want %q, %v", test.input, gotLang, gotArgs, test.wantLang, test.wantArgs)
		}
	}
}

func TestExtractLanguageArgsRejectsInvalidShape(t *testing.T) {
	for _, args := range [][]string{{"--lang"}, {"--lang="}, {"--lang", "en", "--lang", "zh-CN"}} {
		if _, _, err := extractLanguageArgs(args); err == nil {
			t.Fatalf("extractLanguageArgs(%v) unexpectedly succeeded", args)
		}
	}
}

func TestConfigPathFromArgs(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"run", "-config", "/one.yaml"}, want: "/one.yaml"},
		{args: []string{"status", "--config=/two.yaml"}, want: "/two.yaml"},
		{args: []string{"run", "-config=/one.yaml", "--config", "/last.yaml"}, want: "/last.yaml"},
		{args: []string{"run", "--", "-config", "/ignored.yaml"}},
	}
	for _, test := range tests {
		if got := configPathFromArgs(test.args); got != test.want {
			t.Fatalf("configPathFromArgs(%v) = %q, want %q", test.args, got, test.want)
		}
	}
}

func TestCommandUsesSystemConfigOnlyForSystemScopedCommands(t *testing.T) {
	for _, test := range []struct {
		command string
		args    []string
		want    bool
	}{
		{command: "service", args: []string{"status"}, want: true},
		{command: "svc", args: []string{"install"}, want: true},
		{command: "init", args: []string{"--install-service"}, want: true},
		{command: "init", args: []string{"--", "--install-service"}},
		{command: "init", args: []string{"--start"}},
		{command: "run"},
	} {
		if got := commandUsesSystemConfig(test.command, test.args); got != test.want {
			t.Fatalf("commandUsesSystemConfig(%q, %v) = %t, want %t", test.command, test.args, got, test.want)
		}
	}
}

func TestLoadConfiguredLanguageUsesExplicitOrDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	chinesePath := filepath.Join(dir, "zh.yaml")
	invalidPath := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(chinesePath, []byte("version: 1\nlanguage: zh\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidPath, []byte("version: 1\nlanguage: klingon\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	selection := loadConfiguredLanguageWith("run", []string{"-config", chinesePath}, func() (string, error) {
		return invalidPath, nil
	}, config.Load)
	if selection.Locale != i18n.SimplifiedChinese || selection.Path != chinesePath || selection.Invalid != "" {
		t.Fatalf("explicit config selection = %+v", selection)
	}

	selection = loadConfiguredLanguageWith("run", nil, func() (string, error) {
		return invalidPath, nil
	}, config.Load)
	if selection.Locale != i18n.English || selection.Invalid != "klingon" || selection.Path != invalidPath {
		t.Fatalf("invalid default config selection = %+v", selection)
	}
	var warning bytes.Buffer
	printInvalidConfiguredLanguage(&warning, selection, i18n.New(i18n.English))
	if !strings.Contains(warning.String(), "klingon") || !strings.Contains(warning.String(), invalidPath) || !strings.Contains(warning.String(), "using en") {
		t.Fatalf("invalid language warning = %q", warning.String())
	}
}

func TestUsageTextIsLocalized(t *testing.T) {
	english := usageTextFor(i18n.New("en"))
	chinese := usageTextFor(i18n.New("zh-CN"))
	if english == chinese || english == "cli.usage" || chinese == "cli.usage" {
		t.Fatalf("usage localization missing\nEnglish: %q\nChinese: %q", english, chinese)
	}
	if !strings.Contains(english, "install|start|uninstall|status") || !strings.Contains(chinese, "install|start|uninstall|status") {
		t.Fatalf("global usage omitted service start\nEnglish: %q\nChinese: %q", english, chinese)
	}
	for _, usage := range []string{
		i18n.New(i18n.English).T("cli.service.usage"),
		i18n.New(i18n.SimplifiedChinese).T("cli.service.usage"),
	} {
		if !strings.Contains(usage, "install|start|uninstall|status") {
			t.Fatalf("service start missing from usage: %q", usage)
		}
	}
}

func TestVersionJSONIsLanguageInvariant(t *testing.T) {
	previousVersion, previousCommit, previousBuildTime := version, commit, buildTime
	version, commit, buildTime = "v-test", "abc123", "2026-07-27T12:00:00Z"
	defer func() {
		version, commit, buildTime = previousVersion, previousCommit, previousBuildTime
	}()

	var englishJSON, chineseJSON bytes.Buffer
	if err := writeVersion(&englishJSON, true, i18n.New(i18n.English)); err != nil {
		t.Fatal(err)
	}
	if err := writeVersion(&chineseJSON, true, i18n.New(i18n.SimplifiedChinese)); err != nil {
		t.Fatal(err)
	}
	if englishJSON.String() != chineseJSON.String() {
		t.Fatalf("version JSON changed with locale\nEnglish:\n%s\nChinese:\n%s", englishJSON.String(), chineseJSON.String())
	}
	var decoded versionInfo
	if err := json.Unmarshal(chineseJSON.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "vmflow" || decoded.Version != "v-test" || decoded.Commit != "abc123" {
		t.Fatalf("version JSON = %+v", decoded)
	}

	var englishText, chineseText bytes.Buffer
	_ = writeVersion(&englishText, false, i18n.New(i18n.English))
	_ = writeVersion(&chineseText, false, i18n.New(i18n.SimplifiedChinese))
	if englishText.String() == chineseText.String() || !strings.Contains(chineseText.String(), "提交") || !strings.Contains(chineseText.String(), "构建时间") {
		t.Fatalf("human version output was not localized\nEnglish:\n%s\nChinese:\n%s", englishText.String(), chineseText.String())
	}
}
