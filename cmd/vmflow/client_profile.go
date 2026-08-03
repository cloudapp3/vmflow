package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/internal/clientconfig"
	"github.com/cloudapp3/vmflow/internal/i18n"
)

type managementDefaults struct {
	Address       string
	Token         string
	ConfigPath    string
	ProfilePath   string
	ProfileLoaded bool
}

func loadManagementDefaults(w io.Writer) managementDefaults {
	return loadManagementDefaultsWithTranslator(w, defaultTranslator())
}

func loadManagementDefaultsWithTranslator(w io.Writer, translator *i18n.Translator) managementDefaults {
	translator = translatorOrEnglish(translator)
	defaults := managementDefaults{
		Address: "http://" + config.DefaultControlListenAddr,
		Token:   strings.TrimSpace(os.Getenv("VMFLOW_CONTROL_TOKEN")),
	}
	addressFromEnv := strings.TrimSpace(os.Getenv("VMFLOW_CONTROL_ADDR"))
	if addressFromEnv != "" {
		defaults.Address = addressFromEnv
	}

	profile, path, err := clientconfig.LoadDefault()
	defaults.ProfilePath = path
	if err != nil {
		if !os.IsNotExist(err) && w != nil {
			fmt.Fprintf(w, "%s\n", translator.Tf("cli.client_profile.warning_ignored", err))
		}
		return defaults
	}
	if addressFromEnv == "" {
		defaults.Address = profile.Address
	}
	if defaults.Token == "" {
		defaults.Token = profile.Token
	}
	defaults.ConfigPath = profile.ConfigPath
	defaults.ProfileLoaded = true
	return defaults
}
