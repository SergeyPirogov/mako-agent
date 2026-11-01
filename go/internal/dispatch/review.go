package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The MR card: the MR's CI, review threads and conflicts; the agent in Mako's terminal fixes them on request.

type reviewLook struct {
	at    time.Time
	check *trackers.ReviewCheck
	err   error
}

// The board asks every 5 s: the code host is asked at most every 20 s per MR.
const reviewFresh = 20 * time.Second

// Review: what the task's MR needs before the merge; nil when there is no MR yet.
func (d *Dispatcher) Review(ctx context.Context, id string) (*trackers.ReviewCheck, error) {
	r, ok := d.Store.Get(id)
	if !ok || r.PRURL == "" {
		return nil, nil
	}
	d.mu.Lock()
	if l, ok := d.reviews[id]; ok && time.Since(l.at) < reviewFresh {
		d.mu.Unlock()
		return l.check, l.err
	}
	d.mu.Unlock()
	desk, err := d.desk(ctx, id)
	var c *trackers.ReviewCheck
	if err == nil {
		c, err = desk.CheckReview(ctx, r.PRURL)
	}
	d.mu.Lock()
	d.reviews[id] = reviewLook{at: time.Now(), check: c, err: err}
	d.mu.Unlock()
	return c, err
}

func (d *Dispatcher) forgetReview(id string) {
	d.mu.Lock()
	delete(d.reviews, id)
	d.mu.Unlock()
}

func (d *Dispatcher) desk(ctx context.Context, id string) (trackers.ReviewDesk, error) {
	r, _ := d.Store.Get(id)
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return nil, fmt.Errorf("tracker %s was removed from the settings", r.Task.TrackerID)
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return nil, err
	}
	host, _, err := d.host(ctx, t, repo)
	if err != nil {
		return nil, err
	}
	desk, ok := host.(trackers.ReviewDesk)
	if !ok {
		return nil, fmt.Errorf("%s cannot show MRs", host.Host())
	}
	return desk, nil
}

// AskAgent hands the agent in Mako's terminal what the MR needs: "ci" (the failed jobs with the end of their
// logs), "comments" (the open review threads), "conflicts", "push" (commit and push, so the MR has the work),
// "open" (open the MR).
func (d *Dispatcher) AskAgent(ctx context.Context, id, kind string) error {
	if _, ok := d.Store.Get(id); !ok {
		return fmt.Errorf("no such task")
	}
	if !d.Live(id) {
		return fmt.Errorf("the agent is not running: press \"Continue\" first")
	}
	text, err := d.request(ctx, id, kind)
	if err != nil {
		return err
	}
	if err := d.sayTo(id, text); err != nil {
		return err
	}
	if kind == "comments" {
		// The reviewers' words, without Mako's request around them; the MR is the one request just read.
		if c, _ := d.Review(ctx, id); c != nil {
			var said []string
			for _, t := range c.Threads {
				said = append(said, strings.TrimSpace(t.Body))
			}
			go d.suggestLesson(id, strings.Join(said, "\n\n"))
		}
	}
	d.asked(id, kind)
	d.Log(id, askedLog[kind])
	return nil
}

// asked: the agent got the request for kind; a review's comments and CI fixes count, for the statistics, as rework.
func (d *Dispatcher) asked(id, kind string) {
	var threads []trackers.Thread
	if kind == "comments" {
		// Before the cache is forgotten: these are the threads the agent was just given.
		if c, _ := d.Review(context.Background(), id); c != nil {
			threads = c.Threads
		}
	}
	d.forgetReview(id)
	switch kind {
	case "comments":
		d.replyToReview(id, threads)
		d.Store.Update(id, func(rec *core.Record) { rec.ReviewFixes++ })
	case "ci":
		d.Store.Update(id, func(rec *core.Record) { rec.CIFixes++ })
	}
}

// replyToReview leaves in the issue a reply to the reviewers: the card shows that their comments were seen
// and are being handled.
func (d *Dispatcher) replyToReview(id string, threads []trackers.Thread) {
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The reviewer left comments on MR %s. The agent sees them and is fixing them", r.PRURL)
	if len(threads) == 0 {
		b.WriteString(".")
	} else {
		b.WriteString(":\n")
		for _, th := range threads {
			where := ""
			if th.Path != "" {
				where = fmt.Sprintf(" (%s:%d)", th.Path, th.Line)
			}
			body := strings.Join(strings.Fields(th.Body), " ")
			if rs := []rune(body); len(rs) > 140 {
				body = string(rs[:140]) + "…"
			}
			fmt.Fprintf(&b, "\n- %s%s: %s", th.Author, where, body)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := t.Comment(ctx, r.Task, b.String()); err != nil {
		d.Fault(id, "the review reply was not added", err)
	}
}

// spending: ctx whose Claude Code calls add what they cost to the task's phase.
func (d *Dispatcher) spending(ctx context.Context, id, phase string) context.Context {
	return writer.WithSpend(ctx, func(s writer.Spend) {
		if _, ok := d.Store.Get(id); !ok { // gone meanwhile: Update would make it again
			return
		}
		d.Store.Update(id, func(r *core.Record) {
			r.AddSpend(phase, s.USD, s.Tokens)
			if phase == core.PhaseWork { // Mako's notes on the work (summary, MR text): not the agent's context
				return
			}
			for _, run := range s.Context {
				run.Phase = phase
				r.SetContext(run)
			}
		})
	})
}

var askedLog = map[string]string{"push": "the agent was asked to push the changes", "open": "the agent was asked to open an MR",
	"conflicts": "the agent was asked to resolve the conflicts", "rebase": "the agent was asked to update the branch from the base", "ci": "the agent was asked to fix CI", "comments": "the agent was asked to fix the comments"}

// request: what the agent is told for kind, as in AskAgent.
func (d *Dispatcher) request(ctx context.Context, id, kind string) (string, error) {
	r, _ := d.Store.Get(id)
	_, base, _ := d.workOf(id)
	branch := branchOf(r)
	switch kind {
	case "push":
		return i18n.TA("agent.push", branch), nil
	case "open":
		return i18n.TA("agent.open", branch, base), nil
	case "rebase":
		return i18n.TA("agent.rebase", branch, base, base, base), nil
	case "conflicts":
		return i18n.TA("agent.conflicts", r.PRURL, base, branch, base), nil
	case "ci", "comments":
		c, err := d.Review(ctx, id)
		if err != nil {
			return "", err
		}
		if c == nil {
			return "", fmt.Errorf("there is no MR yet")
		}
		if kind == "ci" {
			return d.ciRequest(ctx, id, r.PRURL, c)
		}
		return commentsRequest(r.PRURL, branch, c.Threads), nil
	}
	return "", fmt.Errorf("unknown request %q", kind)
}

func (d *Dispatcher) ciRequest(ctx context.Context, id, url string, c *trackers.ReviewCheck) (string, error) {
	if len(c.Failed) == 0 {
		return "", fmt.Errorf("there are no failed jobs in CI")
	}
	desk, err := d.desk(ctx, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(i18n.TA("agent.ci", url) + "\n")
	for i, j := range c.Failed {
		b.WriteString("\n" + i18n.TA("agent.ciJob", j.Name, j.URL) + "\n")
		if i >= 2 {
			continue // the first ones' logs are enough to start with
		}
		if log, err := desk.JobLog(ctx, url, j, 60); err == nil && log != "" {
			b.WriteString(i18n.TA("agent.ciLog") + "\n```\n" + log + "\n```\n")
		}
	}
	return b.String(), nil
}

func commentsRequest(url, branch string, threads []trackers.Thread) string {
	var b strings.Builder
	b.WriteString(i18n.TA("agent.comments", url, branch) + "\n")
	for i, t := range threads {
		where := ""
		if t.Path != "" {
			where = fmt.Sprintf(" (%s:%d)", t.Path, t.Line)
		}
		fmt.Fprintf(&b, "\n%d. %s%s: %s\n", i+1, t.Author, where, strings.TrimSpace(t.Body))
	}
	return b.String()
}

// MergeReview merges the task's MR; the next round sees it merged and finishes the task.
func (d *Dispatcher) MergeReview(ctx context.Context, id string) error {
	r, ok := d.Store.Get(id)
	if !ok || r.PRURL == "" {
		return fmt.Errorf("there is no MR")
	}
	desk, err := d.desk(ctx, id)
	if err != nil {
		return err
	}
	// Asked afresh, not from the card's cache: a push a moment ago may have started CI again.
	if c, err := desk.CheckReview(ctx, r.PRURL); err == nil && trackers.CIRunning(c.CI) {
		d.forgetReview(id)
		return fmt.Errorf("CI is still running: you can merge once it finishes")
	}
	if err := desk.Merge(ctx, r.PRURL); err != nil {
		return err
	}
	d.forgetReview(id)
	d.Log(id, "MR merged from the board")
	d.Wake()
	return nil
}

// CloseReview closes the task's MR without merging; the next round sees it closed and the task fails.
func (d *Dispatcher) CloseReview(ctx context.Context, id string) error {
	r, ok := d.Store.Get(id)
	if !ok || r.PRURL == "" {
		return fmt.Errorf("there is no MR")
	}
	desk, err := d.desk(ctx, id)
	if err != nil {
		return err
	}
	if err := desk.Close(ctx, r.PRURL); err != nil {
		return err
	}
	d.forgetReview(id)
	d.Log(id, "MR closed from the board without merging")
	d.Wake()
	return nil
}
