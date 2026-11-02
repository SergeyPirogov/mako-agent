package dispatch

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// Routes: each phase of a task runs with the model and the effort of the settings' table for the task's size
// (core.Route); the mode moves the whole table, a stuck task's agent goes up a level (escalate), a model picked
// for the task weighs more than the table. A route changes only between runs: a running session keeps its own.

// agentPhases: the runs of the task's own agent, the ones escalation lifts.
var agentPhases = []string{core.PhasePlan, core.PhaseWork, core.PhaseCIFix}

// sizeOf: the task's size for its route: the estimate's, when the settings take it, else medium.
func sizeOf(cfg *config.Config, r core.Record) (string, bool) {
	if cfg.EstimateModel && estimateFresh(r.Estimate, r.Task) && slices.Contains(core.Sizes, r.Estimate.Size) {
		return r.Estimate.Size, true
	}
	return "medium", false
}

// routeFor: the route of the task's phase. mode: the routes' mode of the task's repo ("" the global one).
func routeFor(cfg *config.Config, r core.Record, phase, mode string) core.Route {
	table := cfg.Routes
	if table == nil {
		table = core.DefaultRoutes()
	}
	size, estimated := sizeOf(cfg, r)
	route := table[phase][size]
	if mode == "" {
		mode = cfg.RouteMode
	}
	route = core.Step(route, core.ModeShift(mode))
	why := []string{size}
	if !estimated {
		why[0] = "medium (no estimate)"
	}
	if mode == core.ModeCost || mode == core.ModeQuality {
		why = append(why, mode)
	}
	agent := slices.Contains(agentPhases, phase)
	if agent && r.Escalation > 0 {
		route = core.Step(route, r.Escalation)
		why = append(why, "escalated: "+strings.Join(r.EscalatedFor, ", "))
	}
	picked := r.Model
	if phase == core.PhaseReview {
		picked = firstOf(cfg.Reviewer.Model, r.Model)
	}
	if picked != "" && (agent || phase == core.PhaseReview) {
		route.Model = picked
		why = append(why, "model picked")
	}
	route.Why = strings.Join(why, ", ")
	return route
}

// routeOf: the route of the task's phase, with its repo's mode.
func (d *Dispatcher) routeOf(r core.Record, phase string) core.Route {
	mode := ""
	if repo := d.repoOf(r); repo != nil {
		mode = repo.RouteMode
	}
	return routeFor(d.Config(), r, phase, mode)
}

// noteRoute keeps the route a phase of the task runs with, for the card and the stats.
func (d *Dispatcher) noteRoute(id, phase string, route core.Route) {
	d.Store.Update(id, func(r *core.Record) { r.SetRoute(phase, route) })
}

// runRoute: the route a one-shot run of the task's phase starts with, kept in its metrics.
func (d *Dispatcher) runRoute(r core.Record, phase string) core.Route {
	route := d.routeOf(r, phase)
	d.noteRoute(r.ID, phase, route)
	return route
}

// escalate lifts the task's next agent runs a level: effort first, then the model (core.Step).
func (d *Dispatcher) escalate(id, why string) {
	r := d.Store.Update(id, func(r *core.Record) {
		r.Escalation++
		r.EscalatedFor = append(slices.Clone(r.EscalatedFor), why)
	})
	d.Log(id, fmt.Sprintf("escalated (%s): the agent's next run is a level up, %s", why, d.routeOf(r, core.PhaseWork).Label()))
}

// runPhase: the phase the agent's next run is for: its plan until it is approved, the one asked for (a CI fix),
// else its work.
func runPhase(r core.Record) string {
	switch {
	case r.Planning:
		return core.PhasePlan
	case r.RunPhase != "":
		return r.RunPhase
	}
	return core.PhaseWork
}

// effortArgs: claude's --effort for the effort ("" none).
func effortArgs(effort string) []string {
	if effort == "" {
		return nil
	}
	return []string{"--effort", effort}
}

// orcaModel: the model Orca's agent is asked for: its work's route (Orca takes no effort).
func (d *Dispatcher) orcaModel(r core.Record) string {
	route := d.routeOf(r, core.PhaseWork)
	d.noteRoute(r.ID, core.PhaseWork, core.Route{Model: route.Model, Why: route.Why})
	return route.Model
}

// askPhase: the phase of the agent's run that handles an MR request of kind.
func askPhase(kind string) string {
	if kind == "ci" || kind == "conflicts" {
		return core.PhaseCIFix
	}
	return core.PhaseWork
}

// seenCI counts a failed CI the agent is asked to fix (kind "ci"): the second in a row escalates the fix.
func (d *Dispatcher) seenCI(id, kind string) {
	if kind != "ci" {
		return
	}
	if r := d.Store.Update(id, func(r *core.Record) { r.CIRed++ }); r.CIRed == 2 {
		d.escalate(id, "CI red ×2")
	}
}

// deliver gives the agent Mako's message for a run of phase. A running agent gets it in its session, unless its
// turn is over and the run needs another model or effort: then its session is closed and resumed with them (a
// new model reads the conversation anew, so only between runs). A closed agent is resumed with it.
func (d *Dispatcher) deliver(id, text, phase string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return fmt.Errorf("no such task")
	}
	if !d.Live(id) {
		d.Store.Update(id, func(r *core.Record) { r.RunPhase = phase })
		return d.resume(id, text)
	}
	next := d.routeOf(r, phase)
	if !restartFor(r, next, d.agentIdle(id), orca.HasTranscript(r.Worktree)) {
		return d.sayTo(id, text)
	}
	d.Log(id, fmt.Sprintf("the agent goes on with %s instead of %s: its session is resumed", next.Label(), r.AgentRoute.Label()))
	d.StopTerminal(id)
	for i := 0; i < 50 && d.Live(id); i++ {
		time.Sleep(100 * time.Millisecond) // one claude at a time on a conversation
	}
	d.Store.Update(id, func(r *core.Record) { r.RunPhase = phase })
	return d.resume(id, text)
}

// restartFor: the running agent is closed and its session resumed for the next run's route: it runs with another
// model or effort, its turn is over (idle), it is not planning, and its conversation is kept (without one a resume
// would start the task over).
func restartFor(r core.Record, next core.Route, idle, kept bool) bool {
	return r.AgentRoute != nil && !sameRun(*r.AgentRoute, next) && idle && !r.Planning && kept
}

// sameRun: the routes start claude the same way.
func sameRun(a, b core.Route) bool { return a.Model == b.Model && a.Effort == b.Effort }
