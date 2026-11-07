package core

import (
	"testing"
	"time"
)

func TestSummarizeIntake(t *testing.T) {
	at := "2026-11-01T10:00:00Z"
	spent := func(usd float64) *Metrics { return &Metrics{Cost: map[string]float64{PhaseIntake: usd}} }
	big := &Estimate{State: "done", Size: "big"}
	records := []Record{
		// Merged tasks: the median of a big one is $4, of a medium one (no estimate) $1.
		{Status: Done, ReviewState: "merged", Cost: 3, Estimate: big},
		{Status: Done, ReviewState: "merged", Cost: 5, Estimate: big},
		{Status: Done, ReviewState: "merged", Cost: 1},
		{Status: Done, ReviewState: "merged", Cost: 1},
		// Intakes.
		{Status: HandedOff, Intake: &Intake{At: at, State: "done", Decision: IntakeTake}, Metrics: spent(0.1)},
		{Status: Asking, Intake: &Intake{At: at, State: "done", Decision: IntakeClarify}, Metrics: spent(0.1), Estimate: big},
		{Status: Done, Intake: &Intake{At: at, State: "done", Decision: IntakeAnswer}, Metrics: spent(0.1)},
		{Status: HandedOff, Intake: &Intake{At: at, State: "done", Decision: IntakeSplit, Overridden: true}, Metrics: spent(0.1)},
		{Status: HandedOff, Intake: &Intake{At: at, State: "failed"}, Metrics: spent(0.1)},
		{Status: Asking, Intake: &Intake{At: "2026-10-01T10:00:00Z", State: "done", Decision: IntakeClarify}},
	}
	st := SummarizeIntake(records, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	want := IntakeStats{Ran: 5, Take: 1, Clarify: 1, Answer: 1, Split: 1, Failed: 1, Filtered: 2, Overridden: 1, Priced: 2}
	got := st
	got.Cost, got.Avoided, got.Saved = 0, 0, 0
	if got != want {
		t.Fatalf("%+v, want %+v", got, want)
	}
	// Avoided: a big clarify ($4) and an answer with no estimate, medium ($1); the overridden split saved nothing.
	if !near(st.Cost, 0.5) || !near(st.Avoided, 4+1) || !near(st.Saved, 4.5) {
		t.Fatalf("cost %v, avoided %v, saved %v", st.Cost, st.Avoided, st.Saved)
	}
}
