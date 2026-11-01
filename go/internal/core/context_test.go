package core

import (
	"fmt"
	"testing"
	"time"
)

func TestSetContext(t *testing.T) {
	r := Record{}
	r.SetContext(ContextRun{Session: "s", Phase: PhasePlan, Peak: 1})
	r.SetContext(ContextRun{Session: "s", Phase: PhaseWork, Peak: 2})
	shared := r.Metrics
	r.SetContext(ContextRun{Session: "s", Phase: PhaseWork, Peak: 3}) // the same session again: counted anew
	if len(r.Metrics.Context) != 2 || r.Metrics.Context[1].Peak != 3 || shared.Context[1].Peak != 2 {
		t.Fatalf("%+v / %+v", r.Metrics.Context, shared.Context)
	}
	for i := 0; i < 40; i++ {
		r.SetContext(ContextRun{Phase: PhaseReview, Peak: 100 + i}) // no session: each its own
	}
	if n := len(r.Metrics.Context); n != maxContextRuns || r.Metrics.Context[n-1].Peak != 139 {
		t.Fatalf("%d runs", n)
	}
	// Mako's own runs went first: the agent's plan and work stayed.
	if c := r.Metrics.Context; c[0].Phase != PhasePlan || c[1].Phase != PhaseWork || c[2].Peak != 112 {
		t.Fatalf("%+v", c[:3])
	}
	if ContextWindow("opus", 150_000) != 200_000 || ContextWindow("claude-opus-4-7[1m]", 1) != 1_000_000 || ContextWindow("", 300_000) != 1_000_000 {
		t.Fatal("window")
	}
}

func TestSummarizeContext(t *testing.T) {
	run := func(phase string, start, peak, compactions int, files ...Heavy) ContextRun {
		return ContextRun{Phase: phase, Start: start, Prompt: 100, Peak: peak, Window: 200_000, Compactions: compactions,
			Input: 10, CacheRead: 900, CacheCreate: 90, Tools: map[string]int{"Read": peak / 2}, Files: files}
	}
	rec := func(at string, runs ...ContextRun) Record {
		return Record{Status: Done, StartedAt: at, Metrics: &Metrics{Context: runs}}
	}
	st := Summarize([]Record{
		rec("2026-10-07T10:00:00Z", run(PhaseWork, 20000, 100000, 1, Heavy{"a.go", 2, 500}), run(PhaseReview, 15000, 30000, 0)),
		rec("2026-10-07T11:00:00Z", run(PhaseWork, 30000, 60000, 0, Heavy{"a.go", 1, 100}, Heavy{"b.go", 1, 900})),
		rec("2026-10-07T12:00:00Z", run(PhaseWork, 40000, 80000, 2)),
		rec("2026-09-01T10:00:00Z", run(PhaseWork, 1, 1, 9)), // out of the period
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	w := st.Context.Phases[PhaseWork]
	if w == nil || w.Runs != 3 || w.Start != 30000 || w.Peak != 80000 || w.PeakShare != 0.4 || w.MaxPeak != 100000 || w.Prompt != 100 {
		t.Fatalf("%+v", w)
	}
	if w.Compactions != 3 || w.Compacted != 2 || w.CacheRead != 2700 || w.Tools["Read"] != 120000 {
		t.Fatalf("%+v", w)
	}
	if st.Context.Phases[PhaseReview].Runs != 1 {
		t.Fatalf("%+v", st.Context.Phases)
	}
	if got := fmt.Sprint(st.Context.Files); got != "[{b.go 1 900} {a.go 3 600}]" {
		t.Fatal(got)
	}
}
