package core

import (
	"encoding/json"
	"testing"
)

// A state.json from before the steps: the advice is one sentence and the tick has only its time and commit.
// It still reads: the sentence is one step, and the tick is there.
func TestOldSelfCheckReads(t *testing.T) {
	old := `{"id":"a","assessment":{"at":"x","state":"done","clarity":{"value":4},"complexity":{"value":2},"completeness":{"value":5},"risk":{"value":3},
		"review":{"need":true,"why":"тонке місце","check":"Відкрити /settings і зберегти"}},"selfCheck":{"at":"2026-11-01T10:00:00Z","commit":"abc"}}`
	var r Record
	if err := json.Unmarshal([]byte(old), &r); err != nil {
		t.Fatal(err)
	}
	steps := r.Assessment.Review.CheckSteps()
	if len(steps) != 1 || steps[0].Do != "Відкрити /settings і зберегти" || steps[0].Text() != steps[0].Do {
		t.Fatalf("steps %+v", steps)
	}
	if r.SelfCheck == nil || r.SelfCheck.Commit != "abc" || len(r.SelfCheck.Steps) != 0 {
		t.Fatalf("self-check %+v", r.SelfCheck)
	}
	// The steps win over the sentence; no advice is no steps.
	a := &ReviewAdvice{Check: "old", Steps: []CheckStep{{Open: "/", Do: "click", Expect: "it opens"}, {Do: "run"}}}
	if got := a.CheckSteps(); len(got) != 2 || got[0].Text() != "/: click → it opens" {
		t.Fatalf("steps %+v", got)
	}
	if StepsText(a.Steps) != "1. /: click → it opens 2. run" {
		t.Fatalf("text %q", StepsText(a.Steps))
	}
	if (*ReviewAdvice)(nil).CheckSteps() != nil || (&ReviewAdvice{}).CheckSteps() != nil {
		t.Fatal("steps out of nothing")
	}
}
