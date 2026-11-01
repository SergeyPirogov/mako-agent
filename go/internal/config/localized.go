package config

import (
	"encoding/json"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// Localized: a text of the settings the developer writes in both languages, {"en": "…", "uk": "…"}. An older
// config's plain string stays what it was in either language: the comment the developer wrote (or «-», none)
// does not change when Mako's language does.
type Localized struct {
	EN string `json:"en,omitempty"`
	UK string `json:"uk,omitempty"`
}

func (l *Localized) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*l = Localized{EN: s, UK: s}
		return nil
	}
	type plain Localized
	return json.Unmarshal(data, (*plain)(l))
}

// In: the version in lang, trimmed; "" when it is not written. «-» (none) in one language is «-» in both: a
// comment turned off stays off whatever the language.
func (l Localized) In(lang string) string {
	en, uk := strings.TrimSpace(l.EN), strings.TrimSpace(l.UK)
	if en == "-" || uk == "-" {
		return "-"
	}
	if lang == i18n.UK {
		return uk
	}
	return en
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
