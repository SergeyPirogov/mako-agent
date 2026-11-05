package dispatch

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Answers to the estimate's questions, written on the card: one comment in the issue, then the estimate again,
// now with the answers among the comments it reads.

var questionMark = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+`)

// SplitQuestions: the estimate's Markdown points, one question each; a line without a marker continues the one
// before it.
func SplitQuestions(md string) []string {
	var qs []string
	for _, line := range strings.Split(md, "\n") {
		text := strings.TrimSpace(line)
		switch {
		case text == "":
		case questionMark.MatchString(line):
			qs = append(qs, strings.TrimSpace(questionMark.ReplaceAllString(line, "")))
		case len(qs) > 0:
			qs[len(qs)-1] += " " + text
		default:
			qs = append(qs, text)
		}
	}
	out := qs[:0]
	for _, q := range qs {
		if q != "" {
			out = append(out, q)
		}
	}
	return out
}

// openQuestions: the estimate's questions for the agent, each with what it assumes; none when the estimate is not
// of the task as it is now. Those answered on the card are in the task's comments, which win over the assumption.
func openQuestions(r core.Record) []orca.Question {
	e := r.Estimate
	if e == nil || e.State != "done" || !estimateFresh(e, r.Task) {
		return nil
	}
	var out []orca.Question
	for i, q := range SplitQuestions(e.Questions) {
		a := ""
		if i < len(e.Assumptions) {
			a = e.Assumptions[i]
		}
		out = append(out, orca.Question{Q: q, Assume: a})
	}
	return out
}

// allAssumed: every question of the estimate comes with what to assume, so none has to wait for an answer.
func allAssumed(e *core.Estimate) bool {
	qs := SplitQuestions(e.Questions)
	if len(qs) == 0 || len(e.Assumptions) < len(qs) {
		return len(qs) == 0
	}
	for _, a := range e.Assumptions[:len(qs)] {
		if strings.TrimSpace(a) == "" {
			return false
		}
	}
	return true
}

// Answer: one question of the estimate as the card showed it, and what was written under it.
type Answer struct {
	Q string `json:"q"`
	A string `json:"a"`
}

// AnswersComment: the comment with the questions that have an answer (by the question's text), in the order of
// the questions; "" when none has.
func AnswersComment(questions []string, answers map[string]string) string {
	var lines []string
	for _, q := range questions {
		if a := strings.TrimSpace(answers[q]); a != "" {
			lines = append(lines, strconv.Itoa(len(lines)+1)+". "+q+" — "+a)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "**" + i18n.TA("comment.answers") + "**\n\n" + strings.Join(lines, "\n")
}

// AnswerQuestions: «Надіслати в трекер». The answers go to the issue as one comment, the task keeps it among its
// comments for the agent, and the estimate starts again. Each answer names its question: one written for a
// question the estimate no longer asks (estimated again meanwhile) is not sent under another.
func (d *Dispatcher) AnswerQuestions(ctx context.Context, id string, answers []Answer) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !isNew(r) {
		return errors.New("assessment questions can only be answered in a new task")
	}
	if r.Estimate == nil || strings.TrimSpace(r.Estimate.Questions) == "" {
		return errors.New("the assessment has no questions")
	}
	qs := SplitQuestions(r.Estimate.Questions)
	byQ := map[string]string{}
	for _, a := range answers {
		if strings.TrimSpace(a.A) == "" {
			continue
		}
		if !slices.Contains(qs, strings.TrimSpace(a.Q)) {
			return errors.New("assessment questions changed: review the answers and send again")
		}
		byQ[strings.TrimSpace(a.Q)] = a.A
	}
	text := AnswersComment(qs, byQ)
	if text == "" {
		return errors.New("no answers")
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return errors.New("tracker " + r.Task.TrackerID + " was removed from settings")
	}
	if err := t.Comment(ctx, r.Task, text); err != nil {
		return err
	}
	me, _ := t.Me(ctx)
	if me == "" {
		me = "Mako"
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Task.Comments = append(r.Task.Comments, core.Comment{Author: me, Body: text, Member: true})
	})
	d.Log(id, "answers to the assessment questions sent to the tracker")
	if d.startEstimate(id) == nil {
		d.goEstimate(id)
	}
	return nil
}

// refresh reads the task from the tracker again before its start, so the agent sees the latest comments (the
// answers among them); when the tracker fails or no longer lists it, the stored task stays and the journal says so.
func (d *Dispatcher) refresh(ctx context.Context, t trackers.Tracker, id string) (core.Task, bool) {
	tasks, err := t.FetchMine(ctx)
	if err != nil {
		d.Log(id, "could not re-read the task from the tracker before start, using the saved one: "+err.Error())
		return core.Task{}, false
	}
	for _, task := range tasks {
		if task.ID == id {
			var fresh core.Task
			d.Store.Update(id, func(r *core.Record) {
				r.Task = withComments(task, r.Task)
				fresh = r.Task
			})
			if task.CommentsUnread {
				d.Log(id, "tracker did not return the task comments, the agent will get the saved ones")
			}
			return fresh, true
		}
	}
	d.Log(id, "the task is no longer in tracker search, the agent will get its saved text")
	return core.Task{}, false
}
