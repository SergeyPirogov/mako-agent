// Package i18n: the language Mako speaks — its interface, its notifications and comments, and what it asks the
// agents to write. One dictionary serves Go and the web pages (GET /api/i18n).
package i18n

import (
	"fmt"
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

// IsRussian says the text is written in Russian: it has the letters Ukrainian lacks (ы, э, ъ, ё) and none of
// the ones Russian lacks (і, ї, є, ґ), or far more of the first. A word or two quoted in a Ukrainian or English
// task does not count, nor does a text too short to tell or one mostly in Latin letters.
func IsRussian(text string) bool {
	russian, ukrainian, cyrillic, letters := 0, 0, 0, 0
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) {
			letters++
		}
		switch r {
		case 'ы', 'э', 'ъ', 'ё':
			russian++
		case 'і', 'ї', 'є', 'ґ':
			ukrainian++
		}
		if unicode.Is(unicode.Cyrillic, r) {
			cyrillic++
		}
	}
	if cyrillic < 20 || cyrillic*2 < letters { // too short to tell, or mostly not Cyrillic: an English task
		return false
	}
	return russian >= 1 && ukrainian == 0 || russian >= 3 && russian > 2*ukrainian
}

// AgentNote: what every agent is told about the language it writes in — the replies, comments, commits and
// merge requests — and about tasks written in Russian.
func AgentNote(lang string) string {
	name := EnglishName(lang)
	return "## Language\n" +
		"Write everything people read in " + name + ": your replies, questions, comments, commit messages, and the merge\n" +
		"request's title and description (its section headings too). Code, identifiers and commands stay as they are.\n" +
		"If the task is written in Russian, do not work on it and change nothing: reply, in " + name + ", that it has to be\n" +
		"rewritten in English or Ukrainian, and stop."
}
