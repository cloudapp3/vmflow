package i18n

import (
	"regexp"
	"slices"
	"sort"
	"testing"
)

func TestMatchNormalizesSupportedLocales(t *testing.T) {
	tests := map[string]string{
		"en":              English,
		"en_US.UTF-8":     English,
		"EN-gb":           English,
		"zh":              SimplifiedChinese,
		"zh_CN.UTF-8":     SimplifiedChinese,
		"zh-Hans@variant": SimplifiedChinese,
	}
	for input, want := range tests {
		got, ok := Match(input)
		if !ok || got != want {
			t.Fatalf("Match(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "de", "zh-TW", "C", "POSIX"} {
		if got, ok := Match(input); ok {
			t.Fatalf("Match(%q) = %q, true; want unsupported", input, got)
		}
	}
}

func TestDetectPriorityAndFallback(t *testing.T) {
	env := map[string]string{
		"VMFLOW_LANG": "zh_CN.UTF-8",
		"LC_ALL":      "zh_CN.UTF-8",
		"LANG":        "zh_CN.UTF-8",
	}
	lookup := func(name string) string { return env[name] }
	if got := Detect("en", SimplifiedChinese, lookup); got != English {
		t.Fatalf("explicit locale = %q", got)
	}
	if got := Detect("", English, lookup); got != SimplifiedChinese {
		t.Fatalf("VMFLOW_LANG locale = %q", got)
	}
	delete(env, "VMFLOW_LANG")
	if got := Detect("", SimplifiedChinese, lookup); got != SimplifiedChinese {
		t.Fatalf("configured locale = %q", got)
	}
	if got := Detect("", "", lookup); got != English {
		t.Fatalf("system locale must not override the English default: %q", got)
	}
	env["VMFLOW_LANG"] = "unsupported"
	if got := Detect("", SimplifiedChinese, lookup); got != English {
		t.Fatalf("unsupported environment override = %q", got)
	}
}

func TestTranslatorFallsBackToEnglishThenKey(t *testing.T) {
	zh := New("zh-CN")
	if got := zh.T("cli.label.config"); got != "配置" {
		t.Fatalf("Chinese translation = %q", got)
	}
	if got := zh.T("missing.key"); got != "missing.key" {
		t.Fatalf("missing-key fallback = %q", got)
	}
	if got := New("unsupported").Locale(); got != English {
		t.Fatalf("unsupported locale = %q", got)
	}
}

func TestLocaleCatalogsMatchEnglishKeysAndFormats(t *testing.T) {
	english := loadMessages(English)
	if len(english) == 0 {
		t.Fatal("English locale is empty")
	}
	englishKeys := sortedKeys(english)
	for _, locale := range SupportedLocales() {
		messages := loadMessages(locale)
		if keys := sortedKeys(messages); !slices.Equal(keys, englishKeys) {
			t.Fatalf("%s keys differ from English\nEnglish: %v\n%s: %v", locale, englishKeys, locale, keys)
		}
		for key, englishValue := range english {
			if messages[key] == "" {
				t.Fatalf("%s has an empty value for %q", locale, key)
			}
			if got, want := formatSignature(messages[key]), formatSignature(englishValue); !slices.Equal(got, want) {
				t.Fatalf("%s format signature for %q = %v; want %v", locale, key, got, want)
			}
		}
	}
}

func sortedKeys(messages map[string]string) []string {
	keys := make([]string, 0, len(messages))
	for key := range messages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var formatDirective = regexp.MustCompile(`%(?:\[[0-9]+\])?[-+# 0]*(?:[0-9]+|\*)?(?:\.(?:[0-9]+|\*))?(?:\[[0-9]+\])?[A-Za-z%]`)

func formatSignature(value string) []string {
	matches := formatDirective.FindAllString(value, -1)
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if match == "%%" {
			continue
		}
		result = append(result, match[len(match)-1:])
	}
	return result
}
