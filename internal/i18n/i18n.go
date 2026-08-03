package i18n

import "fmt"

// Translator resolves stable message keys for one locale. It is immutable
// after construction and can be passed safely to independent CLI or TUI runs.
type Translator struct {
	locale   string
	messages map[string]string
	fallback map[string]string
}

// New creates a translator for locale. Unsupported locales use English.
func New(locale string) *Translator {
	matched, ok := Match(locale)
	if !ok {
		matched = English
	}
	messages := loadMessages(matched)
	fallback := messages
	if matched != English {
		fallback = loadMessages(English)
	}
	return &Translator{
		locale:   matched,
		messages: messages,
		fallback: fallback,
	}
}

// T returns the localized value for key, falling back to English and then key.
func (t *Translator) T(key string) string {
	if t == nil {
		return key
	}
	if value, ok := t.messages[key]; ok && value != "" {
		return value
	}
	if value, ok := t.fallback[key]; ok && value != "" {
		return value
	}
	return key
}

// Tf formats the localized value for key with fmt.Sprintf.
func (t *Translator) Tf(key string, args ...any) string {
	return fmt.Sprintf(t.T(key), args...)
}

// Locale returns the canonical locale code used by this translator.
func (t *Translator) Locale() string {
	if t == nil {
		return English
	}
	return t.locale
}
