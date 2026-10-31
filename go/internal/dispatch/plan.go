package dispatch

import (
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

// wantsPlan: the task's agent starts with a plan. With PlanFirst "big", a plan label decides first, then the
// estimate of the task's current text (e), then the length of its description.
func wantsPlan(t config.Terminal, task core.Task, e *core.Estimate) bool {
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
		if e != nil && e.State == "done" && e.Size != "" && e.For == estimateKey(task) {
			return e.Size == "big"
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
