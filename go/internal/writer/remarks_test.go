package writer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestParseRemarks(t *testing.T) {
	got := ParseRemarks(strings.Join([]string{
		"Ось що я знайшов:",
		"- **R1** [треба] `pay/cart.go:42` — порожній кошик падає з panic",
		"  і на nil теж",
		"* R2 [варто] README.md — опиши новий прапорець",
		"R3 [must] — no test for the refund",
		"1. R4 (should) internal/x/y.go:7-9: rename the var",
		"R5 [невідомо] щось",
		"Not a remark at all",
	}, "\n"))
	want := []core.Remark{
		{ID: "R1", Weight: core.Must, File: "pay/cart.go", Line: 42, Text: "порожній кошик падає з panic\nі на nil теж"},
		{ID: "R2", Weight: core.Should, File: "README.md", Text: "опиши новий прапорець"},
		{ID: "R3", Weight: core.Must, Text: "no test for the refund"},
		{ID: "R4", Weight: core.Should, File: "internal/x/y.go", Line: 7, Text: "rename the var"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// Free text, as older reviewers wrote it: no remarks, the notes stay as they are.
	if got := ParseRemarks("- `pay.go`: немає тесту на порожній кошик"); len(got) != 0 {
		t.Fatalf("free text: %+v", got)
	}
	// Mako numbers them on from the task's earlier rounds, whatever the reviewer wrote.
	if n := Numbered(want[:2], 5); n[0].ID != "R5" || n[1].ID != "R6" || want[0].ID != "R1" {
		t.Fatalf("numbered %+v", n)
	}
	if !Clean([]core.Remark{{Weight: core.Should}}) || Clean(want) || !Clean(nil) {
		t.Fatal("clean")
	}
}

func TestParseChecks(t *testing.T) {
	known := map[string]bool{"R1": true, "R2": true, "R3": true}
	got := ParseChecks(strings.Join([]string{
		"- R1: закрито",
		"- **R2** — не закрито: тест досі не перевіряє nil",
		"R3: приймаю пояснення — це поза задачею",
		"R4: closed", // not an earlier remark
		"R1: open",   // the first verdict counts
		"R5 [треба] a.go — новий пункт",
	}, "\n"), known)
	want := []core.Check{
		{ID: "R1", Verdict: core.Closed},
		{ID: "R2", Verdict: core.Open, Note: "тест досі не перевіряє nil"},
		{ID: "R3", Verdict: core.Accepted, Note: "це поза задачею"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got := ParseChecks("R1: closed — fixed in abc\nR2: accepted", known); len(got) != 2 || got[0].Verdict != core.Closed || got[1].Verdict != core.Accepted {
		t.Fatalf("english: %+v", got)
	}
}

func TestParseReport(t *testing.T) {
	text := strings.Join([]string{
		"Зробив, тести зелені.",
		"",
		"R1 — виправлено (додав перевірку, коміт 1a2b3c)",
		"- **R2** — не виправлено: це поведінка з задачі",
		"R3 — частково: лишився README",
		"R9 — виправлено", // no such remark
	}, "\n")
	got := ParseReport(text, []string{"R1", "R2", "R3"})
	want := map[string]Reply{
		"R1": {core.Fixed, "додав перевірку, коміт 1a2b3c"},
		"R2": {core.NotFixed, "це поведінка з задачі"},
		"R3": {core.Partly, "лишився README"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got := ParseReport("R1 — fixed\nR2 - not fixed: by design\nR3: partly", []string{"R1", "R2", "R3"}); got["R1"].Reply != core.Fixed || got["R2"].Reply != core.NotFixed || got["R3"].Reply != core.Partly {
		t.Fatalf("english: %+v", got)
	}
	// No report: the agent only said it is done.
	if got := ParseReport("Готово", []string{"R1"}); len(got) != 0 {
		t.Fatalf("no report: %+v", got)
	}
}

func TestReviewComments(t *testing.T) {
	a := core.AgentReview{State: "notes",
		Remarks: []core.Remark{{ID: "R3", Weight: core.Must, File: "a.go", Line: 4, Text: "nil", Reply: core.Fixed, ReplyNote: "коміт abc"}, {ID: "R4", Weight: core.Should, Text: "назва"}},
		Checks:  []core.Check{{ID: "R1", Verdict: core.Closed}, {ID: "R2", Verdict: core.Open, Note: "досі"}}}
	c := ReviewComment(2, a)
	for _, s := range []string{"2", "R1", "R2", "досі", "R3 [", "`a.go:4`", "R4 ["} {
		if !strings.Contains(c, s) {
			t.Errorf("the review comment has no %q:\n%s", s, c)
		}
	}
	a.Unverified = true
	r := ReplyComment(a, "https://x/1#note_5")
	for _, s := range []string{"https://x/1#note_5", "R3", "коміт abc", "R4", "⚠️"} {
		if !strings.Contains(r, s) {
			t.Errorf("the reply has no %q:\n%s", s, r)
		}
	}
	if p := PriorText(a.Remarks); !strings.Contains(p, "fixed: коміт abc") || !strings.Contains(p, "no report") {
		t.Errorf("prior: %s", p)
	}
}
