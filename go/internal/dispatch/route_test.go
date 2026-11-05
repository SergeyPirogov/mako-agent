package dispatch

import (
	"slices"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

func routeTask(size string) core.Record {
	task := core.Task{ID: "g/a#1", Title: "x", Description: "y"}
	r := core.Record{Task: task}
	if size != "" {
		r.Estimate = &core.Estimate{State: "done", Size: size, Model: "haiku", For: EstimateKey(task)}
	}
	return r
}

func label(r core.Route) string { return r.Label() }

// Each phase by the estimate's size, as the default table says; no estimate (or the settings not taking it) — medium.
func TestRouteForTable(t *testing.T) {
	cfg := &config.Config{EstimateModel: true, Routes: core.DefaultRoutes(), RouteMode: core.ModeBalanced}
	cases := []struct{ phase, size, want string }{
		{core.PhaseEstimate, "big", "haiku · medium"},
		{core.PhaseAssess, "small", "haiku · medium"},
		{core.PhasePlan, "big", "opus · high"},
		{core.PhaseWork, "small", "sonnet · medium"},
		{core.PhaseWork, "medium", "sonnet · high"},
		{core.PhaseWork, "big", "opus · high"},
		{core.PhaseReview, "big", "opus · medium"},
		{core.PhaseCIFix, "big", "sonnet · high"},
		{core.PhaseWork, "", "sonnet · high"},
	}
	for _, c := range cases {
		if got := label(routeFor(cfg, routeTask(c.size), c.phase, "")); got != c.want {
			t.Errorf("%s/%s: %q, want %q", c.phase, c.size, got, c.want)
		}
	}
	// The estimate advised haiku: only its size counts.
	if got := routeFor(cfg, routeTask("big"), core.PhaseWork, ""); got.Model != "opus" || got.Why != "big" {
		t.Errorf("big: %+v", got)
	}
	off := &config.Config{Routes: core.DefaultRoutes()}
	if got := label(routeFor(off, routeTask("big"), core.PhaseWork, "")); got != "sonnet · high" {
		t.Errorf("settings off: %q", got)
	}
	stale := routeTask("big")
	stale.Task.Description = "changed"
	if got := routeFor(cfg, stale, core.PhaseWork, ""); got.Model != "sonnet" || got.Why != "medium (no estimate)" {
		t.Errorf("stale estimate: %+v", got)
	}
	// A config with no table (tests, an old one): the defaults.
	if got := label(routeFor(&config.Config{}, routeTask(""), core.PhaseWork, "")); got != "sonnet · high" {
		t.Errorf("no table: %q", got)
	}
}

// The mode moves the whole table a level; the repo's own mode wins over the global one.
func TestRouteForMode(t *testing.T) {
	cfg := &config.Config{EstimateModel: true, Routes: core.DefaultRoutes(), RouteMode: core.ModeCost}
	if got := label(routeFor(cfg, routeTask("big"), core.PhaseWork, "")); got != "opus · medium" {
		t.Errorf("cost: %q", got)
	}
	if got := label(routeFor(cfg, routeTask("small"), core.PhaseEstimate, "")); got != "haiku · low" {
		t.Errorf("cost estimate: %q", got)
	}
	got := routeFor(cfg, routeTask("big"), core.PhaseWork, core.ModeQuality)
	if got.Label() != "opus · xhigh" || got.Why != "big, quality" {
		t.Errorf("repo quality: %+v", got)
	}
	if got := label(routeFor(cfg, routeTask("medium"), core.PhaseWork, core.ModeBalanced)); got != "sonnet · high" {
		t.Errorf("repo balanced: %q", got)
	}
}

// A model picked for the task weighs more than the route; the effort stays the route's. The reviewer's own model
// from the settings weighs more still. The estimate and the assessment keep their routes.
func TestRouteForPicked(t *testing.T) {
	cfg := &config.Config{EstimateModel: true, Routes: core.DefaultRoutes()}
	r := routeTask("big")
	r.Model = "sonnet"
	if got := routeFor(cfg, r, core.PhaseWork, ""); got.Label() != "sonnet · high" || got.Why != "big, model picked" {
		t.Errorf("work: %+v", got)
	}
	if got := label(routeFor(cfg, r, core.PhaseReview, "")); got != "sonnet · medium" {
		t.Errorf("review: %q", got)
	}
	if got := label(routeFor(cfg, r, core.PhaseEstimate, "")); got != "haiku · medium" {
		t.Errorf("estimate: %q", got)
	}
	cfg.Reviewer.Model = "opus"
	if got := label(routeFor(cfg, r, core.PhaseReview, "")); got != "opus · medium" {
		t.Errorf("reviewer's model: %q", got)
	}
}

// A model picked for the task replaces the route's model, but the mode still moved its effort: the route keeps the
// mode (the stats and the card count it under it), so the card's label says "(quality)" for the effort's step.
func TestRoutePickedKeepsMode(t *testing.T) {
	cfg := &config.Config{EstimateModel: true, Routes: core.DefaultRoutes(), RouteMode: core.ModeQuality}
	r := routeTask("big")
	r.Model = "sonnet"
	got := routeFor(cfg, r, core.PhaseWork, "")
	if got.Label() != "sonnet · xhigh" || got.LabelWithMode() != "sonnet · xhigh (quality)" || got.Mode != core.ModeQuality || got.Why != "big, quality, model picked" {
		t.Errorf("picked in quality: %+v", got)
	}
}

// Escalation lifts the agent's runs, not the estimate, the reviewer or the assessment.
func TestRouteForEscalated(t *testing.T) {
	cfg := &config.Config{EstimateModel: true, Routes: core.DefaultRoutes()}
	r := routeTask("big")
	r.Escalation, r.EscalatedFor = 1, []string{"CI red ×2"}
	got := routeFor(cfg, r, core.PhaseWork, "")
	if got.Label() != "opus · xhigh" || got.Why != "big, escalated: CI red ×2" {
		t.Errorf("work: %+v", got)
	}
	r.Escalation = 2
	if got := label(routeFor(cfg, r, core.PhaseCIFix, "")); got != "opus · xhigh" { // sonnet · high, xhigh, then opus
		t.Errorf("cifix: %q", got)
	}
	for _, p := range []string{core.PhaseEstimate, core.PhaseReview, core.PhaseAssess} {
		if got := routeFor(cfg, r, p, ""); got.Label() != label(cfg.Routes[p]["big"]) {
			t.Errorf("%s escalated: %+v", p, got)
		}
	}
	// A picked model stays; the effort still goes up.
	r.Model, r.Escalation = "sonnet", 1
	if got := label(routeFor(cfg, r, core.PhaseWork, "")); got != "sonnet · xhigh" {
		t.Errorf("picked: %q", got)
	}
}

// The second failed CI in a row escalates once; a green one between them starts over.
func TestSeenCIEscalates(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := New(&config.Config{StateDir: t.TempDir()}, store, nil)
	id := "g/a#1"
	store.Update(id, func(r *core.Record) { r.Task = core.Task{ID: id} })
	d.seenCI(id, "comments")
	d.seenCI(id, "ci")
	if r, _ := store.Get(id); r.CIRed != 1 || r.Escalation != 0 {
		t.Fatalf("first red: %d, escalation %d", r.CIRed, r.Escalation)
	}
	store.Update(id, func(r *core.Record) { r.CIRed = 0 }) // green
	d.seenCI(id, "ci")
	if r, _ := store.Get(id); r.Escalation != 0 {
		t.Fatalf("a green between: escalation %d", r.Escalation)
	}
	d.seenCI(id, "ci")
	d.seenCI(id, "ci")
	r, _ := store.Get(id)
	if r.CIRed != 3 || r.Escalation != 1 || !slices.Equal(r.EscalatedFor, []string{"CI red ×2"}) {
		t.Fatalf("red ×3: %d, escalation %d %v", r.CIRed, r.Escalation, r.EscalatedFor)
	}
}

func TestRunPhase(t *testing.T) {
	if runPhase(core.Record{}) != core.PhaseWork || runPhase(core.Record{RunPhase: core.PhaseCIFix}) != core.PhaseCIFix ||
		runPhase(core.Record{Planning: true, RunPhase: core.PhaseCIFix}) != core.PhasePlan {
		t.Error("run phase")
	}
	if askPhase("ci") != core.PhaseCIFix || askPhase("conflicts") != core.PhaseCIFix || askPhase("comments") != core.PhaseWork {
		t.Error("ask phase")
	}
}

// A running agent is resumed with the next run's route only between its turns, with its conversation kept.
func TestRestartFor(t *testing.T) {
	running := core.Route{Model: "sonnet", Effort: "high", Why: "medium"}
	r := core.Record{AgentRoute: &running}
	other, same := core.Route{Model: "sonnet", Effort: "medium"}, core.Route{Model: "sonnet", Effort: "high", Why: "other words"}
	if !restartFor(r, other, true, true) {
		t.Error("another effort, idle, kept")
	}
	if restartFor(r, same, true, true) {
		t.Error("the same model and effort")
	}
	if restartFor(r, other, false, true) || restartFor(r, other, true, false) {
		t.Error("busy, or no conversation kept")
	}
	if restartFor(core.Record{}, other, true, true) {
		t.Error("route unknown (started before the routes)")
	}
	r.Planning = true
	if restartFor(r, other, true, true) {
		t.Error("planning")
	}
}

// The route keeps the task's size and the mode it came from, and an escalated one its own model and effort, for
// the stats.
func TestRouteForKeepsKind(t *testing.T) {
	cfg := &config.Config{EstimateModel: true, Routes: core.DefaultRoutes(), RouteMode: core.ModeBalanced}
	got := routeFor(cfg, routeTask("small"), core.PhaseWork, core.ModeCost)
	if got.Size != "small" || got.Mode != core.ModeCost || got.Escalated() {
		t.Errorf("cost: %+v", got)
	}
	if got := routeFor(&config.Config{Routes: core.DefaultRoutes()}, routeTask(""), core.PhaseWork, ""); got.Size != "medium" || got.Mode != core.ModeBalanced {
		t.Errorf("no mode: %+v", got)
	}
	r := routeTask("big")
	r.Escalation, r.EscalatedFor = 1, []string{"retry"}
	got = routeFor(cfg, r, core.PhaseWork, "")
	if got.Label() != "opus · xhigh" || got.BaseModel != "opus" || got.BaseEffort != "high" {
		t.Errorf("escalated: %+v", got)
	}
	r.Model = "sonnet"
	if got := routeFor(cfg, r, core.PhaseWork, ""); got.BaseModel != "sonnet" || got.BaseEffort != "high" {
		t.Errorf("escalated, model picked: %+v", got)
	}
	if got := routeFor(cfg, r, core.PhaseReview, ""); got.Escalated() {
		t.Errorf("the reviewer is not escalated: %+v", got)
	}
}

// Orca's agent: its work's route is kept with the size and the mode, without an effort (Orca takes none).
func TestOrcaModelKeepsRoute(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := New(&config.Config{StateDir: t.TempDir(), EstimateModel: true, Routes: core.DefaultRoutes(), RouteMode: core.ModeQuality}, store, nil)
	r := routeTask("small")
	r.ID = "g/a#1"
	r.Escalation, r.EscalatedFor = 1, []string{"retry"}
	store.Update(r.ID, func(x *core.Record) { *x = r })
	if m := d.orcaModel(r); m != "sonnet" {
		t.Fatalf("model %q", m)
	}
	got, _ := store.Get(r.ID)
	if route := got.RouteOf(core.PhaseWork); route.Size != "small" || route.Mode != core.ModeQuality || route.Effort != "" || route.BaseEffort != "" || route.BaseModel != "sonnet" {
		t.Fatalf("kept %+v", route)
	}
}

// RoutesNow: the table of the settings with the global mode applied, model and effort only; none set: the defaults.
func TestRoutesNow(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	table := core.DefaultRoutes()
	table[core.PhaseWork]["big"] = core.Route{Model: "sonnet", Effort: "high"}
	d := New(&config.Config{Routes: table, RouteMode: core.ModeQuality}, store, nil)
	now := d.RoutesNow()
	if got := label(now[core.PhaseWork]["big"]); got != "sonnet · xhigh" {
		t.Errorf("work big in the quality mode: %q", got)
	}
	if got := label(now[core.PhaseWork]["small"]); got != "sonnet · high" {
		t.Errorf("work small in the quality mode: %q", got)
	}
	if table[core.PhaseWork]["big"].Effort != "high" {
		t.Error("the settings' table changed")
	}
	d = New(&config.Config{}, store, nil)
	if got := label(d.RoutesNow()[core.PhaseWork]["big"]); got != "opus · high" {
		t.Errorf("no table: %q", got)
	}
}
