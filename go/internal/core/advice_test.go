package core

import (
	"math"
	"strings"
	"testing"
	"time"
)

func workRoute(size, model, effort string, taken, ended, clean int, cost float64) RouteGroup {
	return RouteGroup{Phase: PhaseWork, Size: size, Model: model, Effort: effort, Taken: taken, Ended: ended, Clean: clean, Cost: cost, Priced: taken}
}

func titles(as []Advice) string {
	var out []string
	for _, a := range as {
		out = append(out, a.Kind+": "+a.Title)
	}
	return strings.Join(out, " | ")
}

// A route that does worse than another tried for the same size is pointed out with the period's saving and the
// value to set; one in use that does well, or one with too few tasks, is not; a weak one is told to go a level up;
// a route changed since, with no finished task, is watched.
func TestCostAdviceRoutes(t *testing.T) {
	now := RouteTable{PhaseWork: {"small": {Model: "sonnet", Effort: "medium"}, "medium": {Model: "sonnet", Effort: "high"}, "big": {Model: "sonnet", Effort: "high"}}}
	routes := []RouteGroup{workRoute("small", "sonnet", "medium", 23, 23, 21, 0.43), workRoute("medium", "sonnet", "high", 128, 128, 111, 0.79),
		workRoute("big", "sonnet", "high", 7, 7, 0, 24.46), workRoute("big", "opus", "high", 14, 14, 8, 9.05)}
	as := CostAdvice(Stats{Routes: routes}, now, ModeBalanced)
	if got := titles(as); got != "warn: Big tasks · Work: Opus · high is cheaper and does better" {
		t.Fatalf("advice %s", got)
	}
	a := as[0]
	if math.Round(a.Save*100) != 10787 || a.Phase != PhaseWork || a.Size != "big" || a.Set == nil || a.Set.Label() != "opus · high" || !a.Rows[0].Now || a.Rows[1].Now {
		t.Errorf("%+v %+v", a, a.Set)
	}
	// In the cost mode the table's value is a level up from the route a task gets.
	if a := CostAdvice(Stats{Routes: routes}, now, ModeCost)[0]; a.Set.Label() != "opus · xhigh" {
		t.Errorf("cost mode: set %s", a.Set.Label())
	}

	// The better one in use: nothing to say.
	now[PhaseWork]["big"] = Route{Model: "opus", Effort: "high"}
	if got := titles(CostAdvice(Stats{Routes: routes}, now, "")); got != "" {
		t.Errorf("the better route in use: %s", got)
	}
	// Changed since to a route with no record: watched.
	now[PhaseWork]["big"] = Route{Model: "opus", Effort: "medium"}
	if got := titles(CostAdvice(Stats{Routes: routes}, now, "")); got != "wait: Big tasks · Work: now Opus · medium, no finished task on it yet" {
		t.Errorf("a new route: %s", got)
	}
	// Too few finished tasks: nothing.
	if got := titles(CostAdvice(Stats{Routes: []RouteGroup{workRoute("big", "sonnet", "high", 2, 2, 0, 30), workRoute("big", "opus", "high", 2, 2, 2, 5)}},
		RouteTable{PhaseWork: {"big": {Model: "sonnet", Effort: "high"}}}, "")); got != "" {
		t.Errorf("too few: %s", got)
	}
	// Weak, nothing else tried: a level up.
	as = CostAdvice(Stats{Routes: []RouteGroup{workRoute("medium", "sonnet", "high", 10, 10, 3, 1)}}, RouteTable{PhaseWork: {"medium": {Model: "sonnet", Effort: "high"}}}, "")
	if titles(as) != "warn: Medium tasks · Work: Sonnet · high gets 30% through first time" || as[0].Set.Label() != "sonnet · xhigh" {
		t.Errorf("weak: %s %+v", titles(as), as[0].Set)
	}
	// Cheaper but worse: not offered.
	if got := titles(CostAdvice(Stats{Routes: []RouteGroup{workRoute("medium", "sonnet", "high", 10, 10, 9, 1), workRoute("medium", "haiku", "high", 10, 10, 4, 0.2)}},
		RouteTable{PhaseWork: {"medium": {Model: "sonnet", Effort: "high"}}}, "")); got != "" {
		t.Errorf("cheaper but worse: %s", got)
	}
}

// Big tasks are worth splitting only when a unit of their work costs more than a medium task's: the sizes compare by
// the cost per 100 changed lines, over enough merged tasks of each.
func TestCostAdviceBigTasks(t *testing.T) {
	sizes := func(bigPer, medPer float64, lined int) []SizeGroup {
		return []SizeGroup{{Size: "medium", Cost: 2.44, Priced: 48, Ended: 48, Clean: 32, Per100Lines: medPer, Lined: lined},
			{Size: "big", Cost: 13, Priced: 37, Ended: 36, Clean: 9, Per100Lines: bigPer, Lined: lined}}
	}
	as := CostAdvice(Stats{BySize: sizes(1.13, 0.80, 12)}, nil, "")
	if titles(as) != "tip: Big tasks cost 1.4× as much per line of work" {
		t.Fatalf("advice %s", titles(as))
	}
	// 37 big tasks at $13: $481; at the medium price of the work, 1 − 0.80/1.13 of it less.
	if want := 13 * 37 * (1 - 0.80/1.13); math.Abs(as[0].Save-want) > 0.01 || !strings.Contains(as[0].Text, "29%") {
		t.Errorf("save %.2f, want %.2f; %s", as[0].Save, want, as[0].Text)
	}
	if got := titles(CostAdvice(Stats{BySize: sizes(0.95, 0.80, 12)}, nil, "")); got != "" {
		t.Errorf("about the same per line: %s", got)
	}
	if got := titles(CostAdvice(Stats{BySize: sizes(2, 0.80, 4)}, nil, "")); got != "" {
		t.Errorf("too few tasks with lines: %s", got)
	}
}

// The cost of a unit of work by size: merged tasks with a price and their changed lines only.
func TestPer100Lines(t *testing.T) {
	merged := func(size string, cost float64, added, removed int) Record {
		task := Task{ID: "g/a#1"}
		return Record{Task: task, Status: Done, ReviewState: "merged", Cost: cost, StartedAt: "2026-11-01T10:00:00Z", Changed: &ChangeStat{Added: added, Removed: removed},
			Estimate: &Estimate{State: "done", Size: size}}
	}
	recs := []Record{merged("big", 10, 800, 200), merged("big", 4, 100, 100), merged("big", 5, 0, 0)}
	failed := merged("big", 50, 100, 0)
	failed.ReviewState, failed.Status = "", Failed
	recs = append(recs, failed)
	st := Summarize(recs, time.Now(), time.Time{})
	for _, g := range st.BySize {
		if g.Size == "big" && (g.Lined != 2 || math.Abs(g.Per100Lines-14.0/1200*100) > 1e-9) {
			t.Errorf("big: %d lined, %.4f per 100", g.Lined, g.Per100Lines)
		}
	}
}
