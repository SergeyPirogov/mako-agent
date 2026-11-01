package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnterStageAddsUpTheStages(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var r Record
	r.EnterStage("", at) // nothing open: nothing to end
	if r.Metrics != nil {
		t.Fatalf("metrics %+v", r.Metrics)
	}
	r.EnterStage(StageAgent, at)
	r.EnterStage(StageAgent, at.Add(10*time.Minute)) // the same stage: it goes on
	r.EnterStage(StageYou, at.Add(30*time.Minute))
	r.EnterStage(StageAgent, at.Add(40*time.Minute))

	// A restart: the record comes back from state.json with the open stage.
	data, _ := json.Marshal(r)
	var back Record
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.StageSeconds(at.Add(50 * time.Minute)); got[StageAgent] != 40*60 || got[StageYou] != 10*60 {
		t.Fatalf("now %v", got)
	}
	back.EnterStage(StageCI, at.Add(50*time.Minute))
	back.EnterStage(StageReview, at.Add(65*time.Minute))
	back.EnterStage("", at.Add(125*time.Minute))
	want := map[string]float64{StageAgent: 40 * 60, StageYou: 10 * 60, StageCI: 15 * 60, StageReview: 60 * 60}
	got := back.StageSeconds(at.Add(10 * time.Hour)) // ended: later is the same
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("stages %v, want %v", got, want)
		}
	}
	if back.Metrics.Stage != "" || back.Metrics.StageSince != "" {
		t.Fatalf("still open: %+v", back.Metrics)
	}
}

func TestEnterStageDoesNotChangeACopy(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var r Record
	r.EnterStage(StageAgent, at)
	r.EnterStage(StageYou, at.Add(time.Minute))
	copied := r // as the store hands records out
	r.EnterStage(StageAgent, at.Add(2*time.Minute))
	r.AddSpend(PhaseWork, 1, 10)
	if copied.Metrics.Seconds[StageYou] != 0 || copied.Metrics.Cost != nil || copied.Metrics.Stage != StageYou {
		t.Fatalf("the copy changed: %+v", copied.Metrics)
	}
}

func TestAddSpendByPhase(t *testing.T) {
	r := Record{Cost: 0.5} // spent before the phases were kept
	r.AddSpend(PhaseEstimate, 0.1, 1000)
	r.AddSpend(PhaseWork, 1, 50_000)
	r.AddSpend(PhaseWork, 0.5, 20_000)
	r.AddSpend(PhaseReview, 0, 0) // nothing: no change
	m := r.Metrics
	if m.Cost[PhaseEstimate] != 0.1 || m.Cost[PhaseWork] != 1.5 || len(m.Cost) != 2 {
		t.Fatalf("cost %v", m.Cost)
	}
	if m.Tokens[PhaseWork] != 70_000 || r.TokensUsed() != 71_000 {
		t.Fatalf("tokens %v", m.Tokens)
	}
	if r.Cost != 2.1 {
		t.Fatalf("total %v", r.Cost)
	}
}

func TestSummarizeStagesAndPhases(t *testing.T) {
	merged := func(ci, fixes int, m *Metrics) Record {
		return Record{Status: Done, StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T11:00:00Z", PRURL: "u",
			ReviewState: "merged", MergedAt: "2026-10-01T15:00:00Z", CIFixes: ci, ReviewFixes: fixes, Metrics: m}
	}
	st := Summarize([]Record{
		merged(2, 1, &Metrics{Seconds: map[string]float64{StageAgent: 3600, StageCI: 600, StageReview: 7200},
			Cost: map[string]float64{PhaseEstimate: 0.2, PhaseWork: 2}, Tokens: map[string]int{PhaseWork: 100}}),
		merged(0, 0, &Metrics{Seconds: map[string]float64{StageAgent: 1800, StageYou: 1200},
			Cost: map[string]float64{PhaseReview: 0.3}, Tokens: map[string]int{PhaseReview: 20}}),
		merged(1, 0, nil), // from before the stages were kept: not in them
		{Status: HandedOff, StartedAt: "2026-10-02T10:00:00Z", Metrics: &Metrics{Cost: map[string]float64{PhasePlan: 0.4}, Tokens: map[string]int{PhasePlan: 5}}},
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	if st.Staged != 2 || st.Stages[StageAgent] != 2700 || st.Stages[StageYou] != 600 || st.Stages[StageCI] != 300 || st.Stages[StageReview] != 3600 {
		t.Fatalf("stages %v over %d", st.Stages, st.Staged)
	}
	if st.Phases[PhaseEstimate] != 0.2 || st.Phases[PhaseWork] != 2 || st.Phases[PhaseReview] != 0.3 || st.Phases[PhasePlan] != 0.4 {
		t.Fatalf("phases %v", st.Phases)
	}
	if st.PhaseTokens[PhaseWork] != 100 || st.Tokens != 125 {
		t.Fatalf("tokens %v, %d", st.PhaseTokens, st.Tokens)
	}
	if st.CIFixes != 1 || st.ReviewFixes != 1.0/3 {
		t.Fatalf("ci fixes %v, review fixes %v", st.CIFixes, st.ReviewFixes)
	}
	if st.Models[0].Tokens != 125 {
		t.Fatalf("models %+v", st.Models)
	}
}
