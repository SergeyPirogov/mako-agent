package dispatch

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
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

// WorkMode: the mode the agent works in once its plan is approved: the configured one.
func WorkMode(t config.Terminal) string {
	if t.PermissionMode == "" || t.PermissionMode == "plan" {
		return "auto"
	}
	return t.PermissionMode
}

// PlanApproval: how the plan's work goes on: the mode and model it is done in ("" the configured mode, the model it
// has), and your notes on the plan, which the agent reads as it starts.
type PlanApproval struct {
	Mode, Model string
	Notes       chat.PlanNotes
}

// ApprovePlan approves the plan the task's agent offers: it leaves plan mode and does what it planned.
func (d *Dispatcher) ApprovePlan(id, requestID string, o PlanApproval) error {
	c := d.Chats.Get(id)
	if c == nil {
		return fmt.Errorf("the agent is not running")
	}
	mode, err := d.approvalMode(o.Mode)
	if err != nil {
		return err
	}
	model := strings.TrimSpace(o.Model)
	notes := o.Notes.Text()
	// Once the agent has its answer the plan is approved, whatever failed after it: the task goes on, the log says.
	partly := c.ApprovePlan(requestID, mode, model, notes)
	if partly != nil && !errors.Is(partly, chat.ErrApprovedButNot) {
		return partly
	}
	if model != "" && c.Model() != model {
		model = "" // it did not reach the agent: the run goes on with the one it has
	}
	approvedAt := time.Now().UTC().Format(time.RFC3339Nano)
	d.Store.Update(id, func(r *core.Record) {
		r.Planning = false
		r.PlanApprovedAt = approvedAt
		// The same session goes on: its route stays the plan's, with the model picked on approval.
		if r.AgentRoute != nil || model != "" {
			route := core.Route{}
			if r.AgentRoute != nil {
				route = *r.AgentRoute
			}
			if model != "" && model != route.Model {
				route.Model, route.Why = model, "picked when the plan was approved"
			}
			r.AgentRoute = &route
			r.SetRoute(core.PhaseWork, route)
		}
		if model != "" {
			r.AgentModel = model
		}
	})
	msg := "plan approved: the agent is carrying it out in mode " + mode
	if model != "" {
		msg += ", model " + model
	}
	if notes != "" {
		msg += "; notes: " + clipText(firstLine(notes), 120)
	}
	d.Log(id, msg)
	if partly != nil {
		d.Log(id, partly.Error())
	}
	return nil
}

// approvalMode: the mode the approved plan is carried out in: the one picked, else the configured work mode.
func (d *Dispatcher) approvalMode(mode string) (string, error) {
	if mode == "" || mode == "plan" {
		return WorkMode(d.Config().Terminal), nil
	}
	if !slices.Contains(chat.Modes, mode) {
		return "", fmt.Errorf("unknown mode %q", mode)
	}
	return mode, nil
}

// RejectPlan turns the plan of the task's agent down: it carries nothing out and waits for you.
func (d *Dispatcher) RejectPlan(id, requestID string) error {
	c := d.Chats.Get(id)
	if c == nil {
		return fmt.Errorf("the agent is not running")
	}
	if err := c.RejectPlan(requestID); err != nil {
		return err
	}
	d.Log(id, "plan rejected")
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
	d.Store.Update(id, func(r *core.Record) { r.PlanRevisions++ })
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

// WillPlan: the task's agent will start with a plan once it leaves the queue: only Mako's own agent plans, as
// wantsPlan decides at its start.
func (d *Dispatcher) WillPlan(r core.Record) bool {
	cfg := d.Config()
	return cfg.Executor == terminalExecutor && !r.NoPlan && wantsPlan(cfg.Terminal, r.Task, r.Estimate, r.PlanFirst)
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
