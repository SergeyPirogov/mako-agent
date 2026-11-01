package config

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestLanguage(t *testing.T) {
	c, err := Parse([]byte(`{}`), nil)
	if err != nil || c.Language != "en" {
		t.Fatalf("default: %v %q", err, c.Language)
	}
	if c.Comments.OnStart != DefaultCommentOnStart.EN || c.Comments.OnReview != DefaultCommentOnReview.EN {
		t.Fatalf("en comments: %+v", c.Comments)
	}
	c, err = Parse([]byte(`{"language": "uk"}`), nil)
	if err != nil || c.Language != "uk" || c.Comments.OnStart != DefaultCommentOnStart.UK {
		t.Fatalf("uk: %v %+v", err, c)
	}
	// A language Mako does not speak costs nothing else: English, and a problem in the settings.
	c, err = Parse([]byte(`{"language": "ru", "wipLimit": 3}`), nil)
	if err != nil || c.Language != "en" || c.UnknownLanguage != "ru" || c.WipLimit != 3 {
		t.Fatalf("ru: %v %+v", err, c)
	}
}

// The templates come in both languages; an older config's plain string is both versions: what the developer
// wrote stays, whatever the language.
func TestLocalizedTemplates(t *testing.T) {
	raw := `{"language": "%s", "comments": {"onStart": {"en": "Took {branch}", "uk": "Взяв {branch}"}, "onReview": "Відкрив MR {mr}"},
	  "executors": {"orca": {"type": "orca", "instructions": {"en": "Finish in English"}}}}`
	for lang, want := range map[string][3]string{
		"en": {"Took {branch}", "Відкрив MR {mr}", "Finish in English"},
		"uk": {"Взяв {branch}", "Відкрив MR {mr}", "Finish in English"}, // no Ukrainian instructions: the English ones
	} {
		c, err := Parse([]byte(fmt.Sprintf(raw, lang)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := [3]string{c.Comments.OnStart, c.Comments.OnReview, c.Orca.Instructions}; got != want {
			t.Errorf("%s: %q", lang, got)
		}
	}
	var l Localized
	if err := json.Unmarshal([]byte(`"Opened {mr}"`), &l); err != nil || l.In("en") != "Opened {mr}" || l.In("uk") != "Opened {mr}" {
		t.Errorf("plain: %+v", l)
	}
	if l := (Localized{EN: "Finish"}); l.Either("uk") != "Finish" || l.In("uk") != "" {
		t.Errorf("fallback: %+v", l)
	}
}

// «-» (no comment) stays off in both languages: an older plain «-», or «-» in one version only.
func TestNoCommentInEitherLanguage(t *testing.T) {
	for _, comments := range []string{`{"onStart": "-"}`, `{"onStart": {"en": "-"}}`, `{"onStart": {"uk": "-", "en": "Took it"}}`} {
		for _, lang := range []string{"en", "uk"} {
			c, err := Parse([]byte(fmt.Sprintf(`{"language": %q, "comments": %s}`, lang, comments)), nil)
			if err != nil || c.Comments.OnStart != "-" {
				t.Errorf("%s %s: %v %q", lang, comments, err, c.Comments.OnStart)
			}
		}
	}
}
