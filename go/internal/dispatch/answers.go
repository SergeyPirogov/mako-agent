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
		return errors.New("немає такої задачі")
	}
	if !isNew(r) {
		return errors.New("відповісти на питання оцінки можна лише в новій задачі")
	}
	if r.Estimate == nil || strings.TrimSpace(r.Estimate.Questions) == "" {
		return errors.New("в оцінці немає питань")
	}
	qs := SplitQuestions(r.Estimate.Questions)
	byQ := map[string]string{}
	for _, a := range answers {
		if strings.TrimSpace(a.A) == "" {
			continue
		}
		if !slices.Contains(qs, strings.TrimSpace(a.Q)) {
			return errors.New("питання оцінки змінились: перегляньте відповіді й надішліть ще раз")
		}
		byQ[strings.TrimSpace(a.Q)] = a.A
	}
	text := AnswersComment(qs, byQ)
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
// answers among them); when the tracker fails or no longer lists it, the stored task stays and the journal says so.
func (d *Dispatcher) refresh(ctx context.Context, t trackers.Tracker, id string) (core.Task, bool) {
	tasks, err := t.FetchMine(ctx)
	if err != nil {
		d.Log(id, "не вдалось перечитати задачу з трекера перед стартом, беру збережену: "+err.Error())
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
				d.Log(id, "трекер не віддав коментарі задачі, агент отримає збережені")
			}
			return fresh, true
		}
	}
	d.Log(id, "задачі вже немає в пошуку трекера, агент отримає її збережений текст")
	return core.Task{}, false
}
