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
	if _, err := Parse([]byte(`{"language": "ru"}`), nil); err == nil {
		t.Fatal("ru accepted")
	}
}

// The templates come in both languages; an older config's plain string is the version in its own language.
func TestLocalizedTemplates(t *testing.T) {
	raw := `{"language": "%s", "comments": {"onStart": {"en": "Took {branch}", "uk": "Взяв {branch}"}, "onReview": "Відкрив MR {mr}"},
	  "executors": {"orca": {"type": "orca", "instructions": {"en": "Finish in English"}}}}`
	for lang, want := range map[string][3]string{
		"en": {"Took {branch}", DefaultCommentOnReview.EN, "Finish in English"},
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
	if err := json.Unmarshal([]byte(`"Opened {mr}"`), &l); err != nil || l.EN != "Opened {mr}" || l.UK != "" {
		t.Errorf("plain English: %+v", l)
	}
	if l.Either("uk") != "Opened {mr}" || l.In("uk") != "" {
		t.Errorf("fallback: %+v", l)
	}
}
