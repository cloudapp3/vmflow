package i18n

import (
	"embed"
	"encoding/json"
	"path"
	"strings"
)

//go:embed locales/*.json locales/*/*.json
var localeFS embed.FS

func loadMessages(locale string) map[string]string {
	messages := map[string]string{}
	mergeMessages(messages, "locales/"+locale+".json")
	directory := "locales/" + locale
	entries, err := localeFS.ReadDir(directory)
	if err != nil {
		return messages
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		mergeMessages(messages, path.Join(directory, entry.Name()))
	}
	return messages
}

func mergeMessages(destination map[string]string, filename string) {
	data, err := localeFS.ReadFile(filename)
	if err != nil {
		return
	}
	var fragment map[string]string
	if err := json.Unmarshal(data, &fragment); err != nil {
		return
	}
	for key, value := range fragment {
		destination[key] = value
	}
}
