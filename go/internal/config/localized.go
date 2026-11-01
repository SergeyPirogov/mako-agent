package config

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// Localized: a text of the settings the developer writes in both languages, {"en": "…", "uk": "…"}. An older
// config's plain string is read as the version in its own language: Ukrainian when it has Cyrillic letters.
type Localized struct {
	EN string `json:"en,omitempty"`
	UK string `json:"uk,omitempty"`
}

func (l *Localized) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*l = Localized{}
		if cyrillic(s) {
			l.UK = s
		} else {
			l.EN = s
		}
		return nil
	}
	type plain Localized
	return json.Unmarshal(data, (*plain)(l))
}

// In: the version in lang, trimmed; "" when it is not written.
func (l Localized) In(lang string) string {
	if lang == i18n.UK {
		return strings.TrimSpace(l.UK)
	}
	return strings.TrimSpace(l.EN)
}

// Either: the version in lang, else the other one — for the agent's instructions, which it follows in any
// language.
func (l Localized) Either(lang string) string {
	if s := l.In(lang); s != "" {
		return s
	}
	if lang == i18n.UK {
		return l.In(i18n.EN)
	}
	return l.In(i18n.UK)
}

func cyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}
