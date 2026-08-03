package i18n

import (
	"os"
	"strings"
)

const (
	English           = "en"
	SimplifiedChinese = "zh-CN"
)

var supportedLocales = []string{English, SimplifiedChinese}

// SupportedLocales returns the canonical locale codes accepted by vmflow.
func SupportedLocales() []string {
	return append([]string(nil), supportedLocales...)
}

// Match normalizes a locale and reports whether vmflow supports it.
func Match(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if index := strings.IndexByte(value, '@'); index >= 0 {
		value = value[:index]
	}
	if index := strings.IndexByte(value, '.'); index >= 0 {
		value = value[:index]
	}
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
	switch value {
	case "en", "en-us", "en-gb", "en-au", "en-ca":
		return English, true
	case "zh", "zh-cn", "zh-hans", "zh-sg":
		return SimplifiedChinese, true
	default:
		return "", false
	}
}

// Detect chooses a locale using this priority:
// explicit, VMFLOW_LANG, configured, English.
func Detect(explicit, configured string, lookupEnv func(string) string) string {
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	if strings.TrimSpace(explicit) != "" {
		if locale, ok := Match(explicit); ok {
			return locale
		}
		return English
	}
	if value := strings.TrimSpace(lookupEnv("VMFLOW_LANG")); value != "" {
		if locale, ok := Match(value); ok {
			return locale
		}
		return English
	}
	if strings.TrimSpace(configured) != "" {
		if locale, ok := Match(configured); ok {
			return locale
		}
		return English
	}
	return English
}
