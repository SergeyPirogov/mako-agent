package dispatch

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Plan first: with Terminal.PlanFirst, Mako's agent starts in Claude Code's plan mode. It explores and writes a
// plan without changing anything; the chat shows the plan with «Затвердити й робити» (the agent goes on in the
// usual mode) and «Поправити» (your notes go back, it plans again).

// planLabels mark a task as big enough for a plan with PlanFirst "big".
var planLabels = []string{"plan", "план"} // cyrillic-ok: legacy label

// bigDescription: a task's description this long (in characters) is big enough for a plan with PlanFirst "big".
const bigDescription = 1500

// wantsPlan: the task's agent starts with a plan. «план спочатку» picked on the card (picked) decides first. With
// PlanFirst "big", then a plan label, then the estimate of the task's current text (e: big, or it advises a
// plan), then the length of its description.
func wantsPlan(t config.Terminal, task core.Task, e *core.Estimate, picked bool) bool {
	if picked {
		return true
	}
	switch t.PlanFirst {
	case "always":
		return true
	case "big":
		for _, l := range task.Labels {
			for _, p := range planLabels {
				if strings.EqualFold(strings.TrimSpace(l), p) {
					return true
				}
			}
		}
		if estimateFresh(e, task) {
			return e.Size == "big" || e.Plan
		}
		return utf8.RuneCountInString(strings.TrimSpace(task.Description)) >= bigDescription
	}
	return false
}

// workMode: the mode the agent works in once its plan is approved: the configured one.
func workMode(t config.Terminal) string {
	if t.PermissionMode == "" || t.PermissionMode == "plan" {
		return "auto"
	}
	return t.PermissionMode
}

// ApprovePlan approves the plan the task's agent offers: it leaves plan mode and does what it planned.
func (d *Dispatcher) ApprovePlan(id, requestID string) error {
	c := d.Chats.Get(id)
	if c == nil {
		return fmt.Errorf("the agent is not running")
	}
	if err := c.ApprovePlan(requestID, workMode(d.Config().Terminal)); err != nil {
		return err
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Planning = false
		if r.AgentRoute != nil {
			r.SetRoute(core.PhaseWork, *r.AgentRoute) // the same session goes on: its route stays the plan's
		}
	})
	d.Log(id, "plan approved: the agent is carrying it out")
	return nil
}

// RevisePlan sends the agent's plan back with your notes.
func (d *Dispatcher) RevisePlan(id, requestID, notes string) error {
	c := d.Chats.Get(id)
	if c == nil {
		return fmt.Errorf("the agent is not running")
	}
	if err := c.RevisePlan(requestID, notes); err != nil {
		return err
	}
	d.Log(id, "plan sent back for changes: "+clipText(firstLine(notes), 120))
	return nil
}

// estimateFresh: e is a finished estimate of the task's current text.
func estimateFresh(e *core.Estimate, task core.Task) bool {
	return e != nil && e.State == "done" && e.Size != "" && estimateFits(e, task)
}

// DefaultModel: the model the task's agent gets when none is picked for it, as StartModel names it.
func (d *Dispatcher) DefaultModel(r core.Record) string {
	r.Model = ""
	return d.StartModel(r)
}

// StartModel: the model the task's agent will start with, as the board shows it before the start: its work's route.
func (d *Dispatcher) StartModel(r core.Record) string {
	return d.routeOf(r, core.PhaseWork).Model
}

// Pick takes the estimate's advice for a task not started yet: model ("" the configured one) and «план спочатку».
func (d *Dispatcher) Pick(id, model string, plan bool) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if r.Worktree != "" || r.Status == core.HandedOff || r.Status == core.Done {
		return errors.New("the agent has already started: change it via \"Retry differently\"")
	}
	d.Store.Update(id, func(r *core.Record) { r.Model, r.PlanFirst = model, plan })
	switch {
	case model != "" && plan:
		d.Log(id, "the agent will start with a plan, model "+model)
	case model != "":
		d.Log(id, "the agent will start with model "+model)
	case plan:
		d.Log(id, "the agent will start with a plan")
	}
	return nil
}
