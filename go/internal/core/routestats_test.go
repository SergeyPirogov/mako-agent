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

// routeOf: the group of a phase and route in st, a zero one when there is none.
func routeOf(st Stats, phase, size, model, effort string) RouteGroup {
	for _, g := range st.Routes {
		if g.Phase == phase && g.Size == size && g.Model == model && g.Effort == effort {
			return g
		}
	}
	return RouteGroup{}
}

// Each phase has its result of its own: the estimate against the assessment, the plan approved first time, the
// reviewer's rounds, the CI fix.
func TestSummarizeRoutePhaseResults(t *testing.T) {
	route := func(model, effort string) Route { return Route{Model: model, Effort: effort, Size: "medium"} }
	task := func(m *Metrics) Record {
		return Record{Status: Done, ReviewState: "merged", StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:10:00Z", Metrics: m}
	}
	estimated := func(size string, risk int) *Estimate {
		return &Estimate{State: "done", Size: size, Risk: Score{Value: risk}}
	}
	assessed := func(size string, risk int) *Assessment {
		return &Assessment{State: "done", Size: size, Risk: Score{Value: risk}}
	}

	// Estimate: a is the same size and risk, b the same size and another risk, c has no assessment, d an assessment
	// with no size (an older one): only the risk is compared.
	a := task(&Metrics{Routes: map[string]Route{PhaseEstimate: route("haiku", "medium")}})
	a.Estimate, a.Assessment = estimated("medium", 2), assessed("medium", 2)
	b := task(&Metrics{Routes: map[string]Route{PhaseEstimate: route("haiku", "medium")}})
	b.Estimate, b.Assessment = estimated("small", 2), assessed("small", 4)
	c := task(&Metrics{Routes: map[string]Route{PhaseEstimate: route("haiku", "medium")}})
	c.Estimate = estimated("big", 3)
	d := task(&Metrics{Routes: map[string]Route{PhaseEstimate: route("haiku", "medium")}})
	d.Estimate, d.Assessment = estimated("big", 3), assessed("", 3)

	// Plan: e approved at once after 5 minutes, f sent back twice and approved after 15, g approved in the
	// terminal (no approval kept): not counted either way.
	e := task(&Metrics{Routes: map[string]Route{PhasePlan: route("opus", "high")}})
	e.PlanApprovedAt = "2026-10-01T10:05:00Z"
	f := task(&Metrics{Routes: map[string]Route{PhasePlan: route("opus", "high")}})
	f.PlanApprovedAt, f.PlanRevisions = "2026-10-01T10:15:00Z", 2
	g := task(&Metrics{Routes: map[string]Route{PhasePlan: route("opus", "high")}})

	// Agent review: h cleaned in two rounds (3 new remarks, 2 fixed; the carried over one is not new), i went three
	// rounds, j was never read to the end (a failed round).
	h := task(&Metrics{Routes: map[string]Route{PhaseReview: route("sonnet", "high")}})
	h.AgentReviews = []AgentReview{
		{State: "notes", Remarks: []Remark{{ID: "R1", Reply: Fixed}, {ID: "R2", Reply: Fixed}, {ID: "R3", Reply: NotFixed}}},
		{State: "clean", Remarks: []Remark{{ID: "R3", Again: true, Reply: Fixed}}},
	}
	i := task(&Metrics{Routes: map[string]Route{PhaseReview: route("sonnet", "high")}})
	i.AgentReviews = []AgentReview{{State: "notes", Remarks: []Remark{{ID: "R1"}}}, {State: "notes", Remarks: []Remark{{ID: "R1", Again: true}}}, {State: "clean"}}
	j := task(&Metrics{Routes: map[string]Route{PhaseReview: route("sonnet", "high")}})
	j.AgentReviews = []AgentReview{{State: "failed"}}

	// CI fix: k merged after one request, l after two, m failed after one, n merged with a request each of two
	// kinds is still one fix.
	k := task(&Metrics{Routes: map[string]Route{PhaseCIFix: route("sonnet", "medium")}})
	k.CIFixes = 1
	l := task(&Metrics{Routes: map[string]Route{PhaseCIFix: route("sonnet", "medium")}})
	l.CIFixes = 2
	m := Record{Status: Failed, StartedAt: "2026-10-01T10:00:00Z", CIFixes: 1, Metrics: &Metrics{Routes: map[string]Route{PhaseCIFix: route("sonnet", "medium")}}}
	n := task(&Metrics{Routes: map[string]Route{PhaseCIFix: route("sonnet", "medium")}})
	n.CIFixes = 1
	// Still going: its request is counted, its outcome is not known.
	o := Record{Status: HandedOff, StartedAt: "2026-10-01T10:00:00Z", CIFixes: 1, Metrics: &Metrics{Routes: map[string]Route{PhaseCIFix: route("sonnet", "medium")}}}

	st := Summarize([]Record{a, b, c, d, e, f, g, h, i, j, k, l, m, n, o}, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Time{})

	if got := routeOf(st, PhaseEstimate, "medium", "haiku", "medium"); got.Taken != 4 || got.Compared != 2 || got.SizeHits != 2 || got.RiskCompared != 3 || got.RiskHits != 2 {
		t.Errorf("estimate %+v", got)
	}
	// Only the work has a first pass and a time to the MR; the other phases do not borrow the task's.
	if got := routeOf(st, PhaseEstimate, "medium", "haiku", "medium"); got.Succeeded != 0 || got.Ended != 0 || got.Clean != 0 || got.MedianSeconds != 0 {
		t.Errorf("estimate has the task's fate %+v", got)
	}
	if got := routeOf(st, PhasePlan, "medium", "opus", "high"); got.Taken != 3 || got.PlanKnown != 2 || got.PlanFirst != 1 || got.PlanMedianSeconds != 600 {
		t.Errorf("plan %+v", got)
	}
	if got := routeOf(st, PhaseReview, "medium", "sonnet", "high"); got.Taken != 3 || got.ReviewTasks != 2 || got.ReviewRounds != 5 || got.ReviewRemarks != 4 || got.ReviewFixed != 2 || got.ReviewQuick != 1 {
		t.Errorf("review %+v", got)
	}
	if got := routeOf(st, PhaseCIFix, "medium", "sonnet", "medium"); got.Taken != 5 || got.CIRequests != 6 || got.CIEnded != 4 || got.CIGreen != 2 {
		t.Errorf("cifix %+v", got)
	}
}

// A route of older versions of Mako, which kept no effort, is counted under the route of the same phase, size
// and model with the most tasks; with none it stays a row of its own, all of it older.
func TestSummarizeRoutesFoldOlder(t *testing.T) {
	task := func(phase, model, effort string, usd float64, minutes int) Record {
		return Record{Status: Done, ReviewState: "merged", StartedAt: "2026-10-01T10:00:00Z",
			FinishedAt: time.Date(2026, 10, 1, 10, minutes, 0, 0, time.UTC).Format(time.RFC3339),
			Metrics:    &Metrics{Routes: map[string]Route{phase: {Model: model, Effort: effort, Size: "big"}}, Cost: map[string]float64{phase: usd}}}
	}
	records := []Record{
		task(PhaseWork, "opus", "high", 2, 10), task(PhaseWork, "opus", "high", 4, 20), task(PhaseWork, "opus", "max", 6, 30),
		task(PhaseWork, "opus", "", 0, 100), task(PhaseWork, "opus", "", 0, 120), // older: no effort, no price
		task(PhaseWork, "sonnet", "", 1, 10),   // older, and no sonnet route with an effort to count it under
		task(PhaseAssess, "haiku", "", 0.5, 5), // another phase: its own
		task(PhaseAssess, "haiku", "low", 0.5, 5),
	}
	st := Summarize(records, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Time{})

	if got := routeOf(st, PhaseWork, "big", "opus", ""); got.Taken != 0 {
		t.Errorf("a route of its own for the older ones: %+v", got)
	}
	// 2 older tasks go to opus · high (2 tasks) rather than opus · max (1): 4 tasks, the price of the 2 priced, the
	// median of 10, 20, 100 and 120 minutes.
	high := routeOf(st, PhaseWork, "big", "opus", "high")
	if high.Taken != 4 || high.Older != 2 || high.Priced != 2 || !near(high.Cost, 3) || high.Ended != 4 || high.MedianSeconds != 60*60 {
		t.Errorf("opus high %+v", high)
	}
	if max := routeOf(st, PhaseWork, "big", "opus", "max"); max.Taken != 1 || max.Older != 0 {
		t.Errorf("opus max %+v", max)
	}
	if only := routeOf(st, PhaseWork, "big", "sonnet", ""); only.Taken != 1 || only.Older != 1 {
		t.Errorf("sonnet with no effort %+v", only)
	}
	if low := routeOf(st, PhaseAssess, "big", "haiku", "low"); low.Taken != 2 || low.Older != 1 || low.Priced != 2 {
		t.Errorf("assess %+v", low)
	}
}
