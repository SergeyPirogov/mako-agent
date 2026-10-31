package writer

import "testing"

func TestReviewNotes(t *testing.T) {
	for in, want := range map[string]string{
		"LGTM":                     "",
		"  lgtm.\n":                "",
		"**LGTM**":                 "",
		"```\n- a.go:3: тест\n```": "- a.go:3: тест",
		"- a.go:3: немає перевірки": "- a.go:3: немає перевірки",
	} {
		got, err := ReviewNotes(in)
		if err != nil || got != want {
			t.Errorf("ReviewNotes(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ReviewNotes("  "); err == nil {
		t.Error("an empty answer passed")
	}
}
