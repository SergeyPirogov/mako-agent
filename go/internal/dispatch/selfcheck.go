package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// «Check it yourself»: the assessment's advice on what to look at yourself before the merge, as steps to tick.
// A tick belongs to the branch's HEAD it was given at: new commits make it stale. Once every step is ticked at the
// same HEAD the check is done; when the setting asks for it (selfCheck: "advised" or "always"), the merge waits
// for that, or for you to skip the check with a reason.

// SelfCheckView is the check as the board shows it.
type SelfCheckView struct {
	// Required: the merge waits for the check (the setting asks for it here); otherwise it is only advice.
	Required bool           `json:"required,omitempty"`
	Steps    []SelfStepView `json:"steps"`
	// Left: the steps not ticked at the branch as it is now; Done: none is left.
	Left int  `json:"left"`
	Done bool `json:"done,omitempty"`
	// Skipped: the reason you gave to merge without the check.
	Skipped string `json:"skipped,omitempty"`
	// Blocks: the check holds the merge now.
	Blocks bool `json:"blocks,omitempty"`
}

// SelfStepView is a step and its tick: Stale, how many commits the branch got after the tick (0: up to date).
type SelfStepView struct {
	core.CheckStep
	Done  bool `json:"done,omitempty"`
	Stale int  `json:"stale,omitempty"`
}

// checkSteps: what the developer is asked to check on the task: the assessment's steps, or (selfCheck "always")
// one step of looking at the change, when the assessment advises nothing. nil while the agent assesses and when
// nothing is asked.
func (d *Dispatcher) checkSteps(r core.Record) []core.CheckStep {
	if Assessing(r) {
		return nil
	}
	if a := r.Assessment; a != nil && a.State == "done" {
		if steps := a.Review.CheckSteps(); len(steps) > 0 {
			return steps
		}
	}
	if d.selfMode() == config.SelfCheckAlways {
		return []core.CheckStep{{Do: i18n.TA("selfcheck.look")}}
	}
	return nil
}

// selfRequired: the merge of the task waits for the check: Mako's own agent with an open MR.
func (d *Dispatcher) selfRequired(r core.Record, steps []core.CheckStep) bool {
	return len(steps) > 0 && d.selfMode() != config.SelfCheckOff && reviewable(r) && r.Status == core.Done
}

// selfMode: the selfCheck setting ("advised" with no config, as in tests of other things).
func (d *Dispatcher) selfMode() string {
	if c := d.Config(); c != nil && c.SelfCheck != "" {
		return c.SelfCheck
	}
	return config.SelfCheckAdvised
}

// selfTicks: the task's ticks laid over steps, one for each, matched by the step's text: a step the assessment
// no longer has loses its tick, a new one has none. A SelfCheck from before the steps (only At and Commit) ticked
// the whole check: every step, at its commit.
func selfTicks(sc *core.SelfCheck, steps []core.CheckStep) []core.SelfStep {
	out := make([]core.SelfStep, len(steps))
	byText := map[string]core.SelfStep{}
	if sc != nil {
		for _, s := range sc.Steps {
			byText[s.Text] = s
		}
	}
	for i, s := range steps {
		text := s.Text()
		out[i] = core.SelfStep{Text: text}
		if t, ok := byText[text]; ok {
			out[i] = t
		} else if sc != nil && len(sc.Steps) == 0 && sc.Commit != "" {
			out[i] = core.SelfStep{Text: text, Done: true, Commit: sc.Commit, At: sc.At}
		}
	}
	return out
}

// selfHead: the branch's HEAD as the board last looked at it ("" when it cannot be read).
func (d *Dispatcher) selfHead(r core.Record) string {
	head, _ := d.lookBranch(r)
	return head
}

// SelfView: the check of the task for the board; nil when nothing is asked of you. It reads no network, and git
// only through the board's caches.
func (d *Dispatcher) SelfView(r core.Record) *SelfCheckView {
	steps := d.checkSteps(r)
	if len(steps) == 0 {
		return nil
	}
	// The branch is looked at only while the MR is open: a merged task's ticks stay as they were given.
	head := ""
	if reviewable(r) {
		head = d.selfHead(r)
	}
	v := &SelfCheckView{Required: d.selfRequired(r, steps)}
	for _, t := range selfTicks(r.SelfCheck, steps) {
		sv := SelfStepView{CheckStep: steps[len(v.Steps)], Done: t.Done}
		if t.Done && head != "" && t.Commit != "" && t.Commit != head {
			sv.Stale = d.behind(r, t.Commit, head)
		}
		if !t.Done || sv.Stale > 0 {
			v.Left++
		}
		v.Steps = append(v.Steps, sv)
	}
	v.Done = v.Left == 0
	if sc := r.SelfCheck; sc != nil && sc.Skipped != nil {
		v.Skipped = sc.Skipped.Reason
	}
	v.Blocks = v.Required && v.Left > 0 && v.Skipped == ""
	return v
}

// SelfBlocks: the check holds the task's merge: how many of its steps are left, of how many.
func (d *Dispatcher) SelfBlocks(r core.Record) (left, total int, blocks bool) {
	v := d.SelfView(r)
	if v == nil || !v.Blocks {
		return 0, 0, false
	}
	return v.Left, len(v.Steps), true
}

// selfAtMerge: how the check stood when the MR went in, for Record.CheckedAtMerge; "" when nothing was asked.
func (d *Dispatcher) selfAtMerge(r core.Record) string {
	if !onTerminal(r) {
		return "" // an agent in Orca: no ticks to give
	}
	v := d.SelfView(r)
	switch {
	case v == nil:
		return ""
	case v.Done:
		return "checked"
	case v.Skipped != "":
		return "skipped"
	}
	return "unchecked"
}

// errSelfStep: a tick the check cannot take.
var errSelfStep = errors.New("no such step of the check")

// headOf: the HEAD of the task's worktree, read now.
func (d *Dispatcher) headOf(id string) (string, error) {
	wt, _, err := d.workOf(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); err != nil || head == "" {
		return "", errors.New("could not read the branch")
	}
	return head, nil
}

// SelfStep ticks step n (from 0) of the task's check at the branch's HEAD, or takes the tick away. Once every step
// is ticked at the same HEAD, the whole check is.
func (d *Dispatcher) SelfStep(id string, n int, on bool, why string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	steps := d.checkSteps(r)
	if n < 0 || n >= len(steps) {
		return errSelfStep
	}
	var head string
	if on {
		if !reviewable(r) {
			return errors.New("the check can only be set for Mako agents that are in progress or in an open MR")
		}
		var err error
		if head, err = d.headOf(id); err != nil {
			return err
		}
	}
	d.Store.Update(id, func(r *core.Record) {
		sc := r.SelfCheck
		if sc == nil {
			sc = &core.SelfCheck{}
		}
		ticks := selfTicks(sc, steps)
		if on {
			ticks[n] = core.SelfStep{Text: ticks[n].Text, Done: true, Commit: head, At: now()}
		} else {
			ticks[n] = core.SelfStep{Text: ticks[n].Text}
		}
		sc.Steps = ticks
		sc.At, sc.Commit = "", ""
		if all := allAt(ticks, head); all && on {
			sc.At, sc.Commit = now(), head
		}
		r.SelfCheck = sc
	})
	switch {
	case !on:
		d.Log(id, fmt.Sprintf("self-check step %d cleared", n+1))
	case why != "":
		d.Log(id, fmt.Sprintf("self-check step %d ticked at %s without a run: %s", n+1, short(head), why))
	default:
		d.Log(id, fmt.Sprintf("self-check step %d ticked at %s", n+1, short(head)))
	}
	return nil
}

// allAt: every tick is given, at head.
func allAt(ticks []core.SelfStep, head string) bool {
	for _, t := range ticks {
		if !t.Done || t.Commit != head {
			return false
		}
	}
	return len(ticks) > 0
}

// short: a commit's short hash for the journal.
func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// SelfCheck ticks the whole check at the branch's HEAD (every step of it), or takes every tick away.
func (d *Dispatcher) SelfCheck(id string, checked bool) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !checked {
		d.Store.Update(id, func(r *core.Record) {
			// What is not a tick stays: the skip with its reason, the MR's task list as last synced.
			if sc := r.SelfCheck; sc != nil && (sc.Skipped != nil || sc.Synced != nil) {
				r.SelfCheck = &core.SelfCheck{Skipped: sc.Skipped, Synced: sc.Synced}
			} else {
				r.SelfCheck = nil
			}
		})
		d.Log(id, "self-check cleared")
		return nil
	}
	if !reviewable(r) {
		return errors.New("the check can only be set for Mako agents that are in progress or in an open MR")
	}
	head, err := d.headOf(id)
	if err != nil {
		return err
	}
	steps := d.checkSteps(r)
	d.Store.Update(id, func(r *core.Record) {
		sc := r.SelfCheck
		if sc == nil {
			sc = &core.SelfCheck{}
		}
		ticks := selfTicks(sc, steps)
		for i := range ticks {
			ticks[i] = core.SelfStep{Text: ticks[i].Text, Done: true, Commit: head, At: now()}
		}
		sc.Steps, sc.At, sc.Commit = ticks, now(), head
		r.SelfCheck = sc
	})
	d.Log(id, "self-check set at "+head)
	return nil
}

// SelfSkip: you merge without the check, for the reason given (it goes to the journal); the merge no longer waits
// for it.
func (d *Dispatcher) SelfSkip(id, why string) error {
	why = strings.TrimSpace(why)
	if why == "" {
		return errors.New("say why the check is skipped")
	}
	if _, ok := d.Store.Get(id); !ok {
		return errors.New("no such task")
	}
	d.Store.Update(id, func(r *core.Record) {
		if r.SelfCheck == nil {
			r.SelfCheck = &core.SelfCheck{}
		}
		r.SelfCheck.Skipped = &core.SelfSkip{At: now(), Reason: why}
	})
	d.Log(id, "self-check skipped: "+why)
	return nil
}

// behind: how many commits head is past from; a commit gone from the branch (a rebase, a force-push) counts as
// 1. The commits between two fixed SHAs never change, so the count is kept by the pair.
func (d *Dispatcher) behind(r core.Record, from, head string) int {
	key := from + ".." + head
	d.mu.Lock()
	n, ok := d.selfBehind[key]
	d.mu.Unlock()
	if ok {
		return n
	}
	n = 1
	if wt, _, err := d.workOf(r.ID); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if out, err := gitx.Run(ctx, wt, "rev-list", "--count", key); err == nil {
			if c, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && c > 0 {
				n = c
			}
		}
		cancel()
	}
	d.mu.Lock()
	if len(d.selfBehind) > 2000 {
		clear(d.selfBehind)
	}
	d.selfBehind[key] = n
	d.mu.Unlock()
	return n
}

// SelfCheckBehind: how many commits the branch got after your «Check it yourself» tick of the whole check; 0 when
// there is no tick or it is up to date.
func (d *Dispatcher) SelfCheckBehind(r core.Record) int {
	sc := r.SelfCheck
	if sc == nil || sc.Commit == "" {
		return 0
	}
	head := d.selfHead(r)
	if head == "" || head == sc.Commit {
		return 0
	}
	return d.behind(r, sc.Commit, head)
}
