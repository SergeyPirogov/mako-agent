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
var planLabels = []string{"plan", "план"}

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
		return fmt.Errorf("агент не запущений")
	}
	if err := c.ApprovePlan(requestID, workMode(d.Config().Terminal)); err != nil {
		return err
	}
	d.Store.Update(id, func(r *core.Record) { r.Planning = false })
	d.Log(id, "план затверджено: агент виконує")
	return nil
}

// RevisePlan sends the agent's plan back with your notes.
func (d *Dispatcher) RevisePlan(id, requestID, notes string) error {
	c := d.Chats.Get(id)
	if c == nil {
		return fmt.Errorf("агент не запущений")
	}
	if err := c.RevisePlan(requestID, notes); err != nil {
		return err
	}
	d.Log(id, "план повернуто на правки: "+clipText(firstLine(notes), 120))
	return nil
}

// estimateFresh: e is a finished estimate of the task's current text.
func estimateFresh(e *core.Estimate, task core.Task) bool {
	return e != nil && e.State == "done" && e.Size != "" && e.For == estimateKey(task)
}

// modelFor: the model of the task's agent: picked for the task (on the card or with «Повторити інакше»), else
// the estimate's advice when the settings take it, else "" (the configured one).
func modelFor(cfg *config.Config, r core.Record) string {
	if r.Model != "" {
		return r.Model
	}
	if cfg.EstimateModel && estimateFresh(r.Estimate, r.Task) {
		return r.Estimate.Model
	}
	return ""
}

// DefaultModel: the model the task's agent gets when none is picked for it: the estimate's advice when the
// settings take it, else "" (the configured one).
func (d *Dispatcher) DefaultModel(r core.Record) string {
	r.Model = ""
	return modelFor(d.Config(), r)
}

// DefaultModel: the model of Mako's agent when nothing else names one.
const DefaultModel = "sonnet"

// StartModel: the model Mako's agent starts a task with: picked for the task, else the estimate's advice when the
// settings take it, else the configured one, else Sonnet.
func StartModel(cfg *config.Config, r core.Record) string {
	if m := modelFor(cfg, r); m != "" {
		return m
	}
	if cfg.Terminal.Model != "" {
		return cfg.Terminal.Model
	}
	return DefaultModel
}

// Pick takes the estimate's advice for a task not started yet: model ("" the configured one) and «план спочатку».
func (d *Dispatcher) Pick(id, model string, plan bool) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("немає такої задачі")
	}
	if r.Worktree != "" || r.Status == core.HandedOff || r.Status == core.Done {
		return errors.New("агент уже почав: змінити можна через «Повторити інакше»")
	}
	d.Store.Update(id, func(r *core.Record) { r.Model, r.PlanFirst = model, plan })
	switch {
	case model != "" && plan:
		d.Log(id, "агент почне з плану, модель "+model)
	case model != "":
		d.Log(id, "агент почне з моделлю "+model)
	case plan:
		d.Log(id, "агент почне з плану")
	}
	return nil
}
