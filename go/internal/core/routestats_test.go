package core

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The routes: tasks by the route of each phase, how they went; the escalations by reason; the what-if from the
// tokens by kind; the modes.
func TestSummarizeRoutes(t *testing.T) {
	work := func(size, model, effort, mode string) Route {
		return Route{Model: model, Effort: effort, Size: size, Mode: mode}
	}
	merged := func(minutes int, m *Metrics) Record {
		return Record{Status: Done, ReviewState: "merged", StartedAt: "2026-10-01T10:00:00Z",
			FinishedAt: time.Date(2026, 10, 1, 10, minutes, 0, 0, time.UTC).Format(time.RFC3339), PRURL: "u", Metrics: m}
	}
	u := Usage{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheCreate: 200_000}
	a := merged(10, &Metrics{Routes: map[string]Route{PhaseWork: work("small", "sonnet", "medium", ModeBalanced), PhaseEstimate: work("small", "haiku", "medium", ModeBalanced)},
		Cost: map[string]float64{PhaseWork: 2.5, PhaseEstimate: 0.1}, Usage: map[string]Usage{PhaseWork: u}, UsageCost: map[string]float64{PhaseWork: 2}})
	b := merged(30, &Metrics{Routes: map[string]Route{PhaseWork: work("small", "claude-sonnet-5-5", "medium", ModeBalanced)}, Cost: map[string]float64{PhaseWork: 3.5}})
	b.ReviewFixes = 1 // not a first pass
	// c and d were lifted from opus · high: they count there, not under the route they were lifted to.
	lifted := work("big", "opus", "max", ModeQuality)
	lifted.BaseModel, lifted.BaseEffort = "opus", "high"
	c := Record{Status: Failed, StartedAt: "2026-10-01T10:00:00Z", Escalation: 2, EscalatedFor: []string{"CI red ×2", "retry"},
		Metrics: &Metrics{Routes: map[string]Route{PhaseWork: lifted}}}
	d := merged(20, &Metrics{Routes: map[string]Route{PhaseWork: work("big", "opus", "high", ModeQuality)}})
	d.Escalation, d.EscalatedFor = 2, []string{"CI red ×2", "CI red ×2"}
	d.Metrics.Routes[PhaseWork] = lifted
	// Escalated before the route kept its own: which route it failed on is unknown, so it is not in the table.
	old := Record{Status: Failed, StartedAt: "2026-10-01T10:00:00Z", Metrics: &Metrics{Routes: map[string]Route{PhaseWork: {Model: "opus", Effort: "xhigh", Why: "big, escalated: retry"}}}}
	st := Summarize([]Record{a, b, c, d, old, {Status: Done, StartedAt: "2026-10-01T10:00:00Z"}}, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Time{})

	if len(st.Routes) != 3 {
		t.Fatalf("routes %+v", st.Routes)
	}
	est, small, big := st.Routes[0], st.Routes[1], st.Routes[2]
	if est.Phase != PhaseEstimate || est.Model != "haiku" || est.Taken != 1 {
		t.Fatalf("estimate first: %+v", st.Routes)
	}
	if small != (RouteGroup{Phase: PhaseWork, Size: "small", Model: "sonnet", Effort: "medium", Taken: 2, Succeeded: 2, Ended: 2, Clean: 1, Cost: 3, Priced: 2, MedianSeconds: 1200}) {
		t.Fatalf("small work %+v", small)
	}
	if big.Size != "big" || big.Model != "opus" || big.Effort != "high" || big.Taken != 2 || big.Ended != 2 || big.Clean != 1 || big.Priced != 0 || big.MedianSeconds != 1200 {
		t.Fatalf("big work %+v", big)
	}

	e := st.Escalations
	if e.Tasks != 2 || e.Ended != 2 || e.Merged != 1 {
		t.Fatalf("escalations %+v", e)
	}
	if len(e.Reasons) != 2 || e.Reasons[0] != (EscalationReason{"ci", 2, 1}) || e.Reasons[1] != (EscalationReason{"retry", 1, 0}) {
		t.Fatalf("reasons %+v", e.Reasons)
	}

	// Only a's work has tokens by kind: opus 1M×4 + 0.1M×20 + 2M×0.2 + 0.2M×8 = 8; sonnet 2 + 1 + 0.2 + 0.8 = 4.
	// Its actual is the $2 of those calls, not the phase's $2.5 with the calls from before.
	if st.WhatIfTasks != 1 || st.WhatIfActual != 2 || !near(st.WhatIf["opus"], 8) || !near(st.WhatIf["sonnet"], 4) {
		t.Fatalf("what-if %v of %v over %d", st.WhatIf, st.WhatIfActual, st.WhatIfTasks)
	}

	if len(st.Modes) != 2 || st.Modes[0].Mode != ModeBalanced || st.Modes[0].Taken != 3 || st.Modes[1].Mode != ModeQuality || st.Modes[1].Taken != 2 {
		t.Fatalf("modes %+v", st.Modes)
	}
}

func TestSummarizeNoRoutes(t *testing.T) {
	st := Summarize([]Record{{Status: Done, StartedAt: "2026-10-01T10:00:00Z"}}, time.Now(), time.Time{})
	if len(st.Routes) != 0 || st.Escalations.Tasks != 0 || st.WhatIfTasks != 0 || len(st.Modes) != 0 || st.Routes == nil {
		t.Fatalf("%+v %+v %v %+v", st.Routes, st.Escalations, st.WhatIf, st.Modes)
	}
}

// The prices match what Claude Code reports: a task's work on claude-opus-5-5 (its transcript's tokens) cost $0.55.
func TestPricesMatchClaudeCode(t *testing.T) {
	if got := DefaultPrices().Cost("claude-opus-5-5", Usage{Input: 14, Output: 8187, CacheRead: 299268, CacheCreate: 41272}); math.Abs(got-0.554) > 0.001 {
		t.Fatalf("opus %v", got)
	}
}

func TestPricesCost(t *testing.T) {
	p := DefaultPrices()
	if got := p.Cost("claude-opus-4-7[1m]", Usage{Input: 1_000_000, Output: 1_000_000}); !near(got, 24) {
		t.Fatalf("opus %v", got)
	}
	if got := p.Cost("gpt-5", Usage{Input: 1_000_000}); got != 0 {
		t.Fatalf("unknown model %v", got)
	}
}

func TestAddSpendUsage(t *testing.T) {
	var r Record
	r.AddSpend(PhaseWork, 1, Usage{Input: 10, Output: 5})
	r.AddSpend(PhaseWork, 1, Usage{CacheRead: 100, CacheCreate: 1})
	r.AddSpend(PhaseWork, 0.5, Usage{}) // no tokens by kind: not in UsageCost
	if r.Metrics.Usage[PhaseWork] != (Usage{Input: 10, Output: 5, CacheRead: 100, CacheCreate: 1}) || r.Metrics.Tokens[PhaseWork] != 116 || r.Metrics.UsageCost[PhaseWork] != 2 {
		t.Fatalf("%+v %v", r.Metrics.Usage, r.Metrics.Tokens)
	}
}

// A route kept before its size and mode were: read from Why.
func TestRouteKind(t *testing.T) {
	for _, c := range []struct{ why, size, mode string }{
		{"big, cost, escalated: retry", "big", ModeCost},
		{"medium (no estimate)", "medium", ModeBalanced},
		{"model picked", "", ""},
	} {
		if size, mode := routeKind(Route{Why: c.why}); size != c.size || mode != c.mode {
			t.Errorf("%q: %q %q", c.why, size, mode)
		}
	}
	if size, mode := routeKind(Route{Size: "small", Mode: ModeQuality, Why: "big"}); size != "small" || mode != ModeQuality {
		t.Errorf("own fields: %q %q", size, mode)
	}
}

// The what-if is at the prices given (the config's), not the defaults.
func TestSummarizePricedPrices(t *testing.T) {
	r := Record{Status: Done, StartedAt: "2026-10-01T10:00:00Z", Metrics: &Metrics{Usage: map[string]Usage{PhaseWork: {Input: 1_000_000, Output: 1_000_000}}, UsageCost: map[string]float64{PhaseWork: 1}}}
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	p := DefaultPrices()
	p["opus"] = Price{Input: 5, Output: 25}
	if st := SummarizePriced([]Record{r}, now, time.Time{}, p); !near(st.WhatIf["opus"], 30) || !near(st.WhatIf["sonnet"], 12) {
		t.Fatalf("config prices %v", st.WhatIf)
	}
	if st := Summarize([]Record{r}, now, time.Time{}); !near(st.WhatIf["opus"], 24) {
		t.Fatalf("default prices %v", st.WhatIf)
	}
}
