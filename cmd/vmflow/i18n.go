package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/appdirs"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/i18n"
)

func defaultTranslator() *i18n.Translator {
	return i18n.New(i18n.English)
}

func translatorOrEnglish(translator *i18n.Translator) *i18n.Translator {
	if translator == nil {
		return defaultTranslator()
	}
	return translator
}

func extractLanguageArgs(args []string) (string, []string, error) {
	cleaned := make([]string, 0, len(args))
	language := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			cleaned = append(cleaned, args[index:]...)
			break
		}
		var value string
		matched := false
		switch {
		case arg == "--lang" || arg == "-lang":
			matched = true
			if index+1 >= len(args) {
				return "", nil, fmt.Errorf("language value is required after %s", arg)
			}
			index++
			value = args[index]
		case strings.HasPrefix(arg, "--lang="):
			matched = true
			value = strings.TrimPrefix(arg, "--lang=")
		case strings.HasPrefix(arg, "-lang="):
			matched = true
			value = strings.TrimPrefix(arg, "-lang=")
		}
		if !matched {
			cleaned = append(cleaned, arg)
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", nil, fmt.Errorf("language value must not be empty")
		}
		if language != "" {
			return "", nil, fmt.Errorf("language may only be specified once")
		}
		language = value
	}
	return language, cleaned, nil
}

func usageTextFor(translator *i18n.Translator) string {
	return translatorOrEnglish(translator).T("cli.usage")
}

type configuredLanguage struct {
	Locale  string
	Invalid string
	Path    string
}

func loadConfiguredLanguage(command string, args []string) configuredLanguage {
	resolver := defaultRuntimeConfigPath
	if commandUsesSystemConfig(command, args) {
		resolver = appdirs.SystemConfigPath
	}
	return loadConfiguredLanguageWith(command, args, resolver, config.Load)
}

func commandUsesSystemConfig(command string, args []string) bool {
	switch command {
	case "service", "svc", initServiceHelperCommand:
		return true
	case "init":
		for _, arg := range args {
			if arg == "--" {
				break
			}
			if arg == "-install-service" || arg == "--install-service" {
				return true
			}
		}
	}
	return false
}

func loadConfiguredLanguageWith(command string, args []string, resolveDefault func() (string, error), load func(string) (config.File, error)) configuredLanguage {
	path := configPathFromArgs(args)
	if path == "" {
		if usesManagementProfileConfig(command) {
			if profile, _, err := clientconfig.LoadDefault(); err == nil {
				path = strings.TrimSpace(profile.ConfigPath)
			}
		}
		if path == "" && resolveDefault != nil {
			resolved, err := resolveDefault()
			if err != nil {
				return configuredLanguage{}
			}
			path = resolved
		}
	}
	path = strings.TrimSpace(path)
	if path == "" || load == nil {
		return configuredLanguage{}
	}
	cfg, err := load(path)
	if err != nil {
		return configuredLanguage{}
	}
	return configuredLanguage{Locale: cfg.Language, Invalid: cfg.InvalidLanguage, Path: path}
}

func usesManagementProfileConfig(command string) bool {
	switch command {
	case "guide", "status", "ctl", "c", "tui", "t", "mcp":
		return true
	default:
		return false
	}
}

func configPathFromArgs(args []string) string {
	path := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		switch {
		case arg == "-config" || arg == "--config":
			if index+1 < len(args) {
				index++
				path = args[index]
			}
		case strings.HasPrefix(arg, "-config="):
			path = strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			path = strings.TrimPrefix(arg, "--config=")
		}
	}
	return strings.TrimSpace(path)
}

func printInvalidConfiguredLanguage(w io.Writer, selection configuredLanguage, translator *i18n.Translator) {
	if w == nil || strings.TrimSpace(selection.Invalid) == "" {
		return
	}
	translator = translatorOrEnglish(translator)
	fmt.Fprintln(w, translator.Tf("cli.warning.invalid_config_language", selection.Invalid, selection.Path))
}

func localizeControlClientFlags(fs *flag.FlagSet, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	keys := map[string]string{
		"tls-ca-file":     "cli.client.flag.tls_ca_file",
		"tls-client-cert": "cli.client.flag.tls_client_cert",
		"tls-client-key":  "cli.client.flag.tls_client_key",
		"tls-skip-verify": "cli.client.flag.tls_skip_verify",
		"H":               "cli.client.flag.header",
		"header":          "cli.client.flag.header_alias",
	}
	for name, key := range keys {
		if item := fs.Lookup(name); item != nil {
			item.Usage = translator.T(key)
		}
	}
}
