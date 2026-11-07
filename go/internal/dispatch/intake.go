package dispatch

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Intake: as a task taken into work starts, before its branch, a cheap agent with tools that only read looks at it
// in the repository's clone and decides whether to work on it at all (config.Intake: never, for unclear and big
// ones, always). "take" goes on as without it; "clarify" asks its questions in the tracker and the task goes back to
// «Нові» until someone answers there (then Mako takes it again by itself); "answer" answers in the tracker and closes
// the task, with no branch and no MR; "split" proposes subtasks (Breakdown). A failure, or an answer Mako cannot
// read, starts the work as without the intake. Taking the task again while its decision stands takes it anyway.

// IntakeFrom: the intake's agent; tests put in their own.
var IntakeFrom = writer.Intake

// intakeCurrent: the decision is still about the task as it is: the same title and description, no comment since
// (Mako's own comment counted). Comments go by their number, not their text: a tracker may give back the Markdown
// of Mako's comment otherwise, and that is no answer.
func intakeCurrent(i *core.Intake, t core.Task) bool {
	return i != nil && i.State == "done" && i.For == textKey(t) && len(t.Comments) <= i.Comments
}

// IntakeFor: the decision i made for the task as it is now (its text and its comments), as the intake keeps it.
func IntakeFor(t core.Task, i core.Intake) core.Intake {
	i.For, i.Comments = textKey(t), len(t.Comments)
	return i
}

// IntakeStands: the intake's decision against the work still stands for the task as it is: nobody took it anyway
// and nothing changed since. «Взяти в роботу» then takes it anyway; auto-take leaves it.
func IntakeStands(r core.Record) bool {
	i := r.Intake
	return intakeCurrent(i, r.Task) && i.Decision != core.IntakeTake && !i.Overridden
}

// wantsIntake: the start of the task goes through the intake. Never for a task that already went to work (a retry,
// one taken again after it failed), a subtask of a split, one made from a written plan or one Mako wrote itself.
func wantsIntake(mode string, r core.Record) bool {
	if mode != config.IntakeUnclear && mode != config.IntakeAlways {
		return false
	}
	if r.StartedAt != "" || r.RetryNote != "" || r.LastAttempt != "" || r.ParentID != "" || r.NoPlan || r.CreatedBy != "" {
		return false
	}
	return mode == config.IntakeAlways || intakeWhy(r) != ""
}

// intakeWhy: why "unclear" sends the task through the intake, "" when it is clear and not big: a plan label, no
// estimate of its current text, an unclear description, or a big task by its estimate.
func intakeWhy(r core.Record) string {
	for _, l := range r.Task.Labels {
		for _, p := range planLabels {
			if strings.EqualFold(strings.TrimSpace(l), p) {
				return "big"
			}
		}
	}
	e := r.Estimate
	if !estimateFresh(e, r.Task) {
		if utf8.RuneCountInString(strings.TrimSpace(r.Task.Description)) >= bigDescription {
			return "big"
		}
		return "not estimated"
	}
	if why := unclearWhy(e); why != "" {
		return why
	}
	if e.Size == "big" || e.Plan || e.Split {
		return "big"
	}
	return ""
}

// intakeGate runs the intake for a task starting (r: with its text fresh from the tracker); whether the start goes
// on. When it does not, the task has its decision and has left the queue, or the start was cancelled.
func (d *Dispatcher) intakeGate(ctx context.Context, t trackers.Tracker, repo *config.Repo, r core.Record) bool {
	id := r.ID
	if i := r.Intake; intakeCurrent(i, r.Task) {
		if IntakeStands(r) {
			d.Store.UpdateIfExists(id, func(r *core.Record) {
				if r.Intake != nil {
					c := *r.Intake
					c.Overridden = true
					r.Intake = &c
				}
			})
			d.Log(id, "taken anyway, past the intake's decision: "+i.Decision)
		}
		return true
	}
	if !wantsIntake(d.Config().Intake, r) {
		return true
	}
	d.step(id, "intake", "the agent decides whether to take the task")
	fetchBase(ctx, repo, func(s string) { d.Log(id, s) })
	_, _, cannot := d.creator(r.Task.TrackerID)
	in, err := IntakeFrom(d.spending(ctx, id, core.PhaseIntake), repo.Path, repo.BaseBranch, d.runRoute(r, core.PhaseIntake), r.Task, cannot == nil)
	if ctx.Err() != nil {
		d.endSteps(id)
		d.cancelled(ctx, id, "", "", "", false)
		return false
	}
	in = IntakeFor(r.Task, in)
	in.At = now()
	if err != nil {
		d.Log(id, "the intake failed, the task goes to work as usual: "+err.Error())
		d.keepIntake(id, core.Intake{At: in.At, For: in.For, Comments: in.Comments, State: "failed", Error: err.Error()})
		return true
	}
	switch in.Decision {
	case core.IntakeClarify:
		return d.intakeClarify(ctx, t, r, in)
	case core.IntakeAnswer:
		return d.intakeAnswer(ctx, t, r, in)
	case core.IntakeSplit:
		d.endSteps(id)
		d.intakeBack(id, in, nil)
		d.Log(id, "the intake found several tasks in one: the agent proposes subtasks; "+in.Why)
		if err := d.SplitTask(id); err != nil {
			d.Log(id, "the subtasks were not proposed: "+err.Error())
		}
		notify.Inform(notify.Stopped, i18n.T("notify.notTaken", id), in.Why, d.BoardURL)
		return false
	}
	d.keepIntake(id, in)
	d.Log(id, "the intake takes the task into work: "+in.Why)
	return true
}

// keepIntake keeps the intake's decision on a task still starting.
func (d *Dispatcher) keepIntake(id string, in core.Intake) {
	d.Store.UpdateIfExists(id, func(r *core.Record) { r.Intake = &in })
}

// intakeBack: the task leaves the queue for «Нові» with the intake's decision (and Mako's comment among its
// comments, counted in the decision), unless it was cancelled meanwhile.
func (d *Dispatcher) intakeBack(id string, in core.Intake, comment *core.Comment) {
	d.Store.UpdateIfExists(id, func(r *core.Record) {
		if r.Status != core.Queued {
			return
		}
		if comment != nil {
			r.Task.Comments = append(r.Task.Comments, *comment)
			in.Comments = len(r.Task.Comments)
		}
		r.Status, r.Intake = core.Asking, &in
	})
}

// intakeComment posts the intake's comment; the comment as the task keeps it, nil when it was not posted (the
// task then goes to work: the decision would be lost unsaid).
func (d *Dispatcher) intakeComment(ctx context.Context, t trackers.Tracker, task core.Task, text string) *core.Comment {
	if err := t.Comment(ctx, task, text); err != nil {
		d.Fault(task.ID, "intake comment not added", err)
		return nil
	}
	me, _ := t.Me(ctx)
	if me == "" {
		me = "Mako"
	}
	return &core.Comment{Author: me, Body: text, Member: true}
}

// intakeClarify asks the intake's questions in the tracker; the task waits in «Нові» for the answers.
func (d *Dispatcher) intakeClarify(ctx context.Context, t trackers.Tracker, r core.Record, in core.Intake) bool {
	id := r.ID
	points := make([]string, len(in.Questions))
	for i, q := range in.Questions {
		points[i] = "- " + q
	}
	c := d.intakeComment(ctx, t, r.Task, i18n.TA("comment.intakeClarify", strings.Join(points, "\n")))
	if c == nil {
		in.Decision = core.IntakeTake
		d.keepIntake(id, in)
		d.Log(id, "the intake would ask in the tracker, but the comment failed: the task goes to work as usual")
		return true
	}
	d.endSteps(id)
	d.intakeBack(id, in, c)
	d.Log(id, "the intake asked in the tracker, the task waits for the answers: "+in.Why)
	notify.Inform(notify.Stopped, i18n.T("notify.notTaken", id), in.Why, d.BoardURL)
	return false
}

// intakeAnswer answers the task in the tracker and closes it there, as a merge would: no branch, no MR.
func (d *Dispatcher) intakeAnswer(ctx context.Context, t trackers.Tracker, r core.Record, in core.Intake) bool {
	id := r.ID
	c := d.intakeComment(ctx, t, r.Task, i18n.TA("comment.intakeAnswer", in.Answer))
	if c == nil {
		in.Decision = core.IntakeTake
		d.keepIntake(id, in)
		d.Log(id, "the intake would answer in the tracker, but the comment failed: the task goes to work as usual")
		return true
	}
	d.endSteps(id)
	status, err := t.Move(ctx, r.Task, trackers.AtMerged)
	at := now()
	answered := false
	d.Store.UpdateIfExists(id, func(r *core.Record) {
		if r.Status != core.Queued {
			return
		}
		answered = true
		r.Task.Comments = append(r.Task.Comments, *c)
		in.Comments = len(r.Task.Comments)
		in.Closed = err == nil
		r.Status, r.FinishedAt, r.Intake = core.Done, at, &in
		switch {
		case err != nil:
			r.TrackerStatus = &core.TrackerState{OK: false, Text: err.Error()}
		case status != "":
			r.TrackerStatus = &core.TrackerState{OK: true, Text: status}
		}
	})
	if !answered {
		return false
	}
	if err != nil {
		d.Fault(id, "the task was answered, but not closed in the tracker", err)
	}
	d.Log(id, "the intake answered the task in the tracker, no branch or MR: "+in.Why)
	return false
}

// intakeAgain: an answer came to the intake's questions: the task is taken again, and the intake looks at it anew.
// Only while the intake is on, and not for a task you put off yourself.
func (d *Dispatcher) intakeAgain(ctx context.Context, r core.Record, fresh core.Task) {
	i := r.Intake
	if i == nil || i.State != "done" || i.Decision != core.IntakeClarify || i.Overridden || r.PostponedByYou || fresh.CommentsUnread {
		return
	}
	if d.Config().Intake == config.IntakeNever || len(fresh.Comments) <= i.Comments {
		return
	}
	d.take(ctx, r.ID, "an answer came in the tracker: taken again, the intake looks at it anew")
}

// intakeReopened: a task the intake answered and closed is open in the tracker again: it goes back to «Нові».
func (d *Dispatcher) intakeReopened(ctx context.Context, t trackers.Tracker, r core.Record, fresh core.Task) {
	i := r.Intake
	if r.Status != core.Done || r.PRURL != "" || i == nil || i.Decision != core.IntakeAnswer || !i.Closed {
		return
	}
	if st, err := t.IssueState(ctx, fresh); err != nil || st != "open" {
		return
	}
	d.Store.Update(r.ID, func(r *core.Record) {
		c := *r.Intake
		c.Closed = false
		r.Task, r.Status, r.FinishedAt, r.ArchivedAt, r.Intake = withComments(fresh, r.Task), core.Snoozed, "", 0, &c
	})
	d.Log(r.ID, "reopened in the tracker after the intake's answer")
	d.maybeEstimate(r.ID)
}
