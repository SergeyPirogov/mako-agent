package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
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
		return nil, fmt.Errorf("трекер %s прибрано з налаштувань", r.Task.TrackerID)
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
		return nil, fmt.Errorf("%s не вміє показувати MR", host.Host())
	}
	return desk, nil
}

// AskAgent hands the agent in Mako's terminal what the MR needs: "ci" (the failed jobs with the end of their
// logs), "comments" (the open review threads), "conflicts", "push" (commit and push, so the MR has the work),
// "open" (open the MR).
func (d *Dispatcher) AskAgent(ctx context.Context, id, kind string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return fmt.Errorf("немає такої задачі")
	}
	s := d.Sessions.Get(id)
	if s == nil || !s.Running() {
		return fmt.Errorf("агент у терміналі не запущений: спершу «Продовжити в терміналі»")
	}
	_, base, _ := d.workOf(id)
	branch := branchOf(r)
	var text string
	switch kind {
	case "push":
		text = fmt.Sprintf("Закоміть зроблене (нормальне повідомлення, як в інструкції) і запуш гілку %s, щоб MR її мав.", branch)
	case "open":
		text = fmt.Sprintf("Закоміть зроблене, запуш гілку %s і відкрий MR у %s, як описано в інструкції до задачі.", branch, base)
	case "conflicts":
		text = fmt.Sprintf("MR %s конфліктує з %s. Зроби rebase гілки %s на origin/%s, розвʼяжи конфлікти, прожени тести й запуш з --force-with-lease.", r.PRURL, base, branch, base)
	case "ci", "comments":
		c, err := d.Review(ctx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("MR ще немає")
		}
		if kind == "ci" {
			text, err = d.ciRequest(ctx, id, r.PRURL, c)
			if err != nil {
				return err
			}
		} else {
			text = commentsRequest(r.PRURL, branch, c.Threads)
		}
	default:
		return fmt.Errorf("невідомий запит %q", kind)
	}
	s.Say(text)
	d.forgetReview(id)
	d.Log(id, map[string]string{"push": "агента попрошено запушити зміни", "open": "агента попрошено відкрити MR",
		"conflicts": "агента попрошено розвʼязати конфлікти", "ci": "агента попрошено полагодити CI", "comments": "агента попрошено виправити коментарі"}[kind])
	return nil
}

func (d *Dispatcher) ciRequest(ctx context.Context, id, url string, c *trackers.ReviewCheck) (string, error) {
	if len(c.Failed) == 0 {
		return "", fmt.Errorf("у CI немає впалих джобів")
	}
	desk, err := d.desk(ctx, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CI на MR %s впав. Знайди причину, полагодь, прожени локально те, що впало, закоміть і запуш.\n", url)
	for i, j := range c.Failed {
		fmt.Fprintf(&b, "\nДжоба %s: %s\n", j.Name, j.URL)
		if i >= 2 {
			continue // the first ones' logs are enough to start with
		}
		if log, err := desk.JobLog(ctx, url, j, 60); err == nil && log != "" {
			b.WriteString("Кінець логу:\n```\n" + log + "\n```\n")
		}
	}
	return b.String(), nil
}

func commentsRequest(url, branch string, threads []trackers.Thread) string {
	var b strings.Builder
	fmt.Fprintf(&b, "На MR %s є нерозвʼязані коментарі рев'ю. Виправ кожен (або поясни, чому ні), закоміть і запуш гілку %s.\n", url, branch)
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
		return fmt.Errorf("MR немає")
	}
	desk, err := d.desk(ctx, id)
	if err != nil {
		return err
	}
	if err := desk.Merge(ctx, r.PRURL); err != nil {
		return err
	}
	d.forgetReview(id)
	d.Log(id, "MR змерджено з дошки")
	d.Wake()
	return nil
}
