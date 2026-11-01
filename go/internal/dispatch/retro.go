package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Retro has the agent look back at a merged task (again): what went well, what did not, what to do differently
// next time. It runs in the background; the board shows it running.
func (d *Dispatcher) Retro(id string) error {
	if err := d.startRetro(id); err != nil {
		return err
	}
	go d.retro(context.Background(), id)
	return nil
}

// startRetro marks the retro as running, unless it cannot or need not start.
func (d *Dispatcher) startRetro(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if r.ReviewState != "merged" {
		return errors.New("a retro is for a merged task")
	}
	if x := r.Retro; x != nil && x.State == "running" && !stale(x.At) {
		return errors.New("the retro is already being written")
	}
	d.Store.Update(id, func(r *core.Record) { r.Retro = &core.Retro{At: now(), State: "running"} })
	return nil
}

// retro writes the task's retro, in the step that saw its MR merged.
func (d *Dispatcher) retro(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	dir := r.Worktree
	if dir == "" || !exists(dir) {
		dir = os.TempDir()
	}
	x, err := writer.Retro(d.spending(ctx, id, core.PhaseAssess), dir, r.Task, d.story(r))
	x.At = now()
	if err != nil {
		d.Fault(id, "the retro was not written", err)
		x = core.Retro{At: x.At, State: "failed", Error: err.Error()}
	} else {
		d.Log(id, "the task retro was written")
	}
	d.Store.Update(id, func(r *core.Record) { r.Retro = &x })
}

// story: how the task went, as Mako saw it, for the retro.
func (d *Dispatcher) story(r core.Record) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("How it went:")
	if r.Attempt > 1 {
		line("- attempts: %d", r.Attempt)
	}
	if t := span(r.StartedAt, r.FinishedAt); t != "" {
		line("- from taking the task to its merge request: %s", t)
	}
	if t := span(r.FinishedAt, r.MergedAt); t != "" {
		line("- from the merge request to its merge: %s", t)
	}
	if r.AutoFixes > 0 {
		line("- times the agent was sent back to fix its merge request (CI, conflicts, review comments): %d", r.AutoFixes)
	}
	if r.Cost > 0 {
		line("- spent: $%.2f", r.Cost)
	}
	if r.RetryNote != "" {
		line("\nThe developer dropped an attempt and asked to do it differently:\n%s", clipText(r.RetryNote, 2000))
	}
	if r.LastAttempt != "" {
		line("\nThe dropped attempt:\n%s", clipText(r.LastAttempt, 2000))
	}
	var said []string
	if s := d.Chats.Get(r.ID); s != nil {
		users := 0
		for _, it := range s.Snapshot().Items {
			if it.Kind == "user" {
				// The first one is the task's prompt.
				if users++; users > 1 && strings.TrimSpace(it.Text) != "" {
					said = append(said, "- "+clipText(strings.TrimSpace(it.Text), 600))
				}
			}
		}
	}
	if len(said) > 0 {
		line("\nMessages the agent got while working (from the developer or Mako):\n%s", lastOf(strings.Join(said, "\n"), 5000))
	}
	for i, v := range r.AgentReviews {
		switch v.State {
		case "notes":
			line("\nReviewer agent, round %d, found:\n%s", i+1, clipText(v.Notes, 1500))
		case "clean":
			line("\nReviewer agent, round %d: nothing to fix", i+1)
		}
	}
	if a := r.Assessment; a != nil && a.State == "done" {
		line("\nRating of the finished task (1-5): description %d (%s), complexity %d, completeness %d, risk %d",
			a.Clarity.Value, a.Clarity.Why, a.Complexity.Value, a.Completeness.Value, a.Risk.Value)
		if a.Missing != "" {
			line("What the description lacked:\n%s", a.Missing)
		}
	}
	if r.Summary != "" {
		line("\nSummary of the work:\n%s", clipText(r.Summary, 3000))
	}
	var journal []string
	for _, l := range d.Logs(r.ID) {
		journal = append(journal, l.At.Format("2006-01-02 15:04")+" "+l.Text)
	}
	if len(journal) > 0 {
		line("\nMako's journal of the task:\n%s", lastOf(strings.Join(journal, "\n"), 4000))
	}
	return b.String()
}

// span: how long from one time to the other, "" when either is missing.
func span(from, to string) string {
	a, err1 := time.Parse(time.RFC3339Nano, from)
	b, err2 := time.Parse(time.RFC3339Nano, to)
	if err1 != nil || err2 != nil || b.Before(a) {
		return ""
	}
	return b.Sub(a).Round(time.Minute).String()
}

// lastOf: the end of s, at most n characters.
func lastOf(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}
