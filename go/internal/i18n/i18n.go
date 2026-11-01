// Package i18n: Mako's interface is in English; what is left of other languages is the agent's — the language
// it is asked to write in (agentLanguage) — and the check for tasks written in Russian. The web pages get their
// English words from GET /api/i18n.
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

// Default: the agent language of a config that names none.
const Default = EN

// Supported says Mako can ask the agents to write in lang.
func Supported(lang string) bool { return lang == EN || lang == UK }

// T: the text for key in the interface's language — English, the only one — formatted with args like
// fmt.Sprintf; the key itself when the dictionary has none. What the agent writes goes through In with the
// settings' agentLanguage.
func T(key string, args ...any) string { return In(EN, key, args...) }

var agentLang atomic.Value // string

// SetAgentLanguage: the language the agents write in from now on (the settings' agentLanguage); an unknown one
// is English.
func SetAgentLanguage(lang string) {
	if !Supported(lang) {
		lang = Default
	}
	agentLang.Store(lang)
}

// AgentLanguage: the language the agents write in now.
func AgentLanguage() string {
	if l, _ := agentLang.Load().(string); l != "" {
		return l
	}
	return Default
}

// TA: T in the agent's language — for what Mako writes on the agent's behalf (merge request texts, review
// reports, tracker comments, commits, the instructions given to the agent).
func TA(key string, args ...any) string { return In(AgentLanguage(), key, args...) }

// In: the text for key in lang, for what the agent writes in its agentLanguage.
func In(lang, key string, args ...any) string {
	var text string
	if pair, ok := dict[key]; ok {
		text = pair[0]
		if lang == UK {
			text = pair[1]
		}
	} else if text, ok = webStrings[key]; !ok {
		return key
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// WebStrings: the words of the web pages (English only), by key.
func WebStrings() map[string]string {
	out := make(map[string]string, len(webStrings))
	for k, v := range webStrings {
		out[k] = v
	}
	return out
}

// EnglishName: what the language is called in English, as the agents are asked to write in it.
func EnglishName(lang string) string {
	if lang == UK {
		return "Ukrainian"
	}
	return "English"
}

// Plural: the form for n in English: one (1) or many.
func Plural(n int, one, few, many string) string {
	if n == 1 {
		return one
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
