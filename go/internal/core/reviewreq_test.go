package core

import (
	"testing"
	"time"
)

func TestAddSpends(t *testing.T) {
	spends := []Spend{
		{At: "2026-11-01T10:00:00Z", Phase: PhaseForMe, Project: "grp/app", USD: 0.5, Usage: Usage{Input: 100, Output: 20}},
		{At: "2026-11-03T10:00:00Z", Phase: PhaseForMe, Project: "grp/app", USD: 0.25, Usage: Usage{Input: 10}},
		{At: "2026-11-03T11:00:00Z", Phase: PhaseForMe, Project: "acme/web", USD: 1},
	}
	st := Summarize(nil, time.Now(), time.Time{})
	AddSpends(&st, spends, time.Time{}, "")
	if st.Phases[PhaseForMe] != 1.75 || st.PhaseTokens[PhaseForMe] != 130 {
		t.Fatalf("all: %v %v", st.Phases, st.PhaseTokens)
	}
	// The period and the project, as the tasks' statistics have them; the tasks' own sums stay theirs.
	st = Summarize(nil, time.Now(), time.Time{})
	AddSpends(&st, spends, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), "grp/app")
	if st.Phases[PhaseForMe] != 0.25 || st.PhaseTokens[PhaseForMe] != 10 || st.Cost != 0 || st.Taken != 0 {
		t.Fatalf("since, project: %+v", st)
	}
	var empty Stats
	AddSpends(&empty, spends[:1], time.Time{}, "")
	if empty.Phases[PhaseForMe] != 0.5 {
		t.Fatalf("no maps: %+v", empty.Phases)
	}
}

func TestDraftClone(t *testing.T) {
	d := &ReviewDraft{Comments: []DraftComment{{ID: "R1", State: DraftOpen}}, Manual: []string{"x"}}
	c := d.Clone()
	c.Comments[0].State, c.Manual[0] = DraftSent, "y"
	if d.Comments[0].State != DraftOpen || d.Manual[0] != "x" {
		t.Fatalf("changed in place: %+v", d)
	}
	if (*ReviewDraft)(nil).Clone() == nil {
		t.Fatal("nil clone")
	}
}
