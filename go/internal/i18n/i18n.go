// Package i18n: the language Mako speaks — its interface, its notifications and comments, and what it asks the
// agents to write. One dictionary serves Go and the web pages (GET /api/i18n).
package i18n

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode"
)

// The languages Mako speaks.
const (
	EN = "en"
	UK = "uk"
)

// Default: the language of a config that names none.
const Default = EN

var current atomic.Value // string

// Supported says Mako speaks lang.
func Supported(lang string) bool { return lang == EN || lang == UK }

// SetLang: the language from now on (the settings' "language"); an unknown one is English.
func SetLang(lang string) {
	if !Supported(lang) {
		lang = Default
	}
	current.Store(lang)
}

// Lang: the language Mako speaks now.
func Lang() string {
	if l, _ := current.Load().(string); l != "" {
		return l
	}
	return Default
}

// T: the text for key in the current language, formatted with args like fmt.Sprintf; the key itself when the
// dictionary has none.
func T(key string, args ...any) string { return In(Lang(), key, args...) }

// In: T in lang.
func In(lang, key string, args ...any) string {
	pair, ok := dict[key]
	if !ok {
		return key
	}
	text := pair[0]
	if lang == UK {
		text = pair[1]
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Strings: the whole dictionary in lang, for the web pages.
func Strings(lang string) map[string]string {
	out := make(map[string]string, len(dict))
	for k := range dict {
		out[k] = In(lang, k)
	}
	return out
}

// Name: what the language is called, in itself.
func Name(lang string) string {
	if lang == UK {
		return "Українська"
	}
	return "English"
}

// EnglishName: what the language is called in English, as the agents are asked to write in it.
func EnglishName(lang string) string {
	if lang == UK {
		return "Ukrainian"
	}
	return "English"
}

// Plural: the form for n in the current language: in English one (1) or many; in Ukrainian one (1, 21), few
// (2-4, 22-24) or many (5-20, 25…).
func Plural(n int, one, few, many string) string {
	if Lang() == EN {
		if n == 1 {
			return one
		}
		return many
	}
	switch {
	case n%10 == 1 && n%100 != 11:
		return one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return few
	}
	return many
}

// IsRussian says the task is written in Russian, word by word. Code, quotes («…», "…") and quoted lines (> …)
// are left out: a log, an error or a user's message pasted into a task is not how its author writes. A text with
// a few Ukrainian words (і, ї, є, ґ, or що, як, або…) is Ukrainian, whatever it quotes; one too short to tell or
// mostly in Latin letters is not Russian. The rest is Russian when its words have the marks of Russian (ы, э, ъ,
// ё, и, что, это, the infinitives in -ать, -ить…) or when it is long and has nothing Ukrainian in it.
func IsRussian(text string) bool {
	text = quoted.ReplaceAllString(text, " ")
	russian, ukrainian, cyrillic, latin := 0, 0, 0, 0
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' && r != 'ʼ' }) {
		w = strings.Trim(w, "'ʼ")
		if w == "" {
			continue
		}
		if !unicode.Is(unicode.Cyrillic, []rune(w)[0]) {
			latin++
			continue
		}
		cyrillic++
		switch {
		case strings.ContainsAny(w, "іїєґ") || ukrainianWords[w]:
			ukrainian++
		case strings.ContainsAny(w, "ыэъё") || russianWords[w] || len([]rune(w)) > 4 && hasAnySuffix(w, "ать", "ять", "ить", "еть"):
			russian++
		}
	}
	if cyrillic < 4 || cyrillic*2 < cyrillic+latin || ukrainian >= 3 {
		return false
	}
	return russian >= 2 || russian >= 1 && ukrainian == 0 || cyrillic >= 8 && ukrainian == 0
}

// quoted: what a task quotes rather than says — code blocks, `code`, «…», "…", “…” and lines starting with >.
var quoted = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`|«[^»]*»|\"[^\"\n]*\"|“[^”]*”|(?m)^\\s*>.*$")

// Words only one of the two languages has.
var (
	russianWords   = setOf("и", "что", "это", "как", "или", "если", "чтобы", "когда", "нужно", "надо", "также", "только", "есть", "его", "был", "была", "было", "были", "они", "она", "он", "ничего", "сейчас", "здесь", "где", "почему", "потому")
	ukrainianWords = setOf("й", "що", "як", "або", "це", "щоб", "треба", "має", "мають", "та", "яке", "якою", "який", "яка", "коли", "також", "тільки", "лише", "він", "вона", "вони", "ми", "ви", "бути", "буде", "його", "зараз", "тут", "де", "чому", "тому", "додай", "зроби", "нема", "немає")
)

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, x := range suffixes {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}

// AgentNote: what every agent is told about the language it writes in — the replies, comments, commits and
// merge requests — and about tasks written in Russian.
func AgentNote(lang string) string {
	name := EnglishName(lang)
	return "## Language\n" +
		"Write everything people read in " + name + ": your replies, questions, comments, commit messages, and the merge\n" +
		"request's title and description. The section headings named in English below (## What changed and the like)\n" +
		"are written in " + name + " too. Code, identifiers and commands stay as they are.\n" +
		"If the task itself is written in Russian, do not work on it and change nothing: reply, in " + name + ", that it has\n" +
		"to be rewritten in English or Ukrainian, and stop. Russian quotes, logs, error messages or interface texts in a\n" +
		"task written in another language are no reason to stop."
}
