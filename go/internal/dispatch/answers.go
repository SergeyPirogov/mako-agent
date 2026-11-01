package dispatch

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
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

// AnswersComment: the comment with the questions that have an answer, "" when none has.
func AnswersComment(questions, answers []string) string {
	var lines []string
	for i, q := range questions {
		if i >= len(answers) {
			break
		}
		if a := strings.TrimSpace(answers[i]); a != "" {
			lines = append(lines, strconv.Itoa(len(lines)+1)+". "+q+" — "+a)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "**Відповіді на питання до задачі:**\n\n" + strings.Join(lines, "\n")
}

// AnswerQuestions: «Надіслати в трекер». The answers (one per question, "" for those left) go to the issue as one
// comment, the task keeps it among its comments for the agent, and the estimate starts again.
func (d *Dispatcher) AnswerQuestions(ctx context.Context, id string, answers []string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("немає такої задачі")
	}
	if !isNew(r) {
		return errors.New("відповісти на питання оцінки можна лише в новій задачі")
	}
	if r.Estimate == nil || strings.TrimSpace(r.Estimate.Questions) == "" {
		return errors.New("в оцінці немає питань")
	}
	text := AnswersComment(SplitQuestions(r.Estimate.Questions), answers)
	if text == "" {
		return errors.New("немає відповідей")
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return errors.New("трекер " + r.Task.TrackerID + " прибрано з налаштувань")
	}
	if err := t.Comment(ctx, r.Task, text); err != nil {
		return err
	}
	me, _ := t.Me(ctx)
	if me == "" {
		me = "Mako"
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Task.Comments = append(r.Task.Comments, core.Comment{Author: me, Body: text})
	})
	d.Log(id, "відповіді на питання оцінки надіслано в трекер")
	if d.startEstimate(id) == nil {
		d.goEstimate(id)
	}
	return nil
}

// refresh reads the task from the tracker again before its start, so the agent sees the latest comments (the
// answers among them); when the tracker fails or no longer lists it, the stored task stays.
func (d *Dispatcher) refresh(ctx context.Context, t trackers.Tracker, id string) (core.Task, bool) {
	tasks, err := t.FetchMine(ctx)
	if err != nil {
		return core.Task{}, false
	}
	for _, task := range tasks {
		if task.ID == id {
			d.Store.Update(id, func(r *core.Record) { r.Task = task })
			return task, true
		}
	}
	return core.Task{}, false
}
