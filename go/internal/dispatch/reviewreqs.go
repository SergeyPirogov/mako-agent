package dispatch

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// «Рев'ю для мене»: the merge requests of others where I am a reviewer, searched in the same round as the
// tasks, with the same tokens.

// keepDoneReviews: how long a finished review stays in «Готово» before it is forgotten.
const keepDoneReviews = 7 * 24 * time.Hour

// pollReviews looks for the tracker's MRs waiting for my review: new ones are told about once, new commits
// mark the card «оновлено», and one merged, closed or approved by me goes to «Готово».
func (d *Dispatcher) pollReviews(ctx context.Context, t trackers.Tracker) {
	inbox, ok := t.(trackers.ReviewInbox)
	if !ok || !t.Config().ReviewRequests || d.Reviews == nil {
		return
	}
	found, err := inbox.ToReview(ctx)
	if err != nil {
		d.Fault(t.ID(), "пошук MR на рев'ю не вдався", err)
		return
	}
	stamp := now()
	seen := map[string]bool{}
	for _, f := range found {
		seen[f.ID] = true
		prev, known := d.Reviews.Get(f.ID)
		if f.Approved {
			if known && prev.State == "open" {
				d.finishReview(f.ID, "approved")
			}
			continue
		}
		reopened := known && prev.State == "done"
		d.Reviews.Update(f.ID, func(r *core.ReviewRequest) {
			since, updated := r.Since, r.Updated
			// New commits since it was last seen; a review asked again after it was done is news too.
			if known && (reopened || prev.HeadSHA != "" && f.HeadSHA != "" && prev.HeadSHA != f.HeadSHA) {
				updated = true
			}
			if !known || reopened {
				since = stamp
			}
			*r = f
			r.Since, r.Updated, r.State = since, updated, "open"
		})
		if !known {
			d.Log(f.ID, "вас призначили рев'ювером: "+f.Title)
			notify.Show(notify.Notice{Kind: notify.ReviewAsked, Title: "👀 Вас призначили рев'ювером: " + f.Ref,
				Message: f.Project + ": " + f.Title, URL: d.reviewLink(f.ID), Tag: "review-asked:" + f.ID})
		}
	}
	// What left the search: merged or closed goes to «Готово»; still open means I am no longer its reviewer.
	for _, r := range d.Reviews.All() {
		if r.TrackerID != t.ID() || r.State != "open" || seen[r.ID] {
			continue
		}
		st, err := inbox.ReviewState(ctx, r.URL)
		switch {
		case err != nil:
		case st == "open":
			d.Log(r.ID, "вас більше не просять рев'ювити, прибираю")
			d.Reviews.Delete(r.ID)
		default:
			d.finishReview(r.ID, st)
		}
	}
	d.forgetDoneReviews(time.Now())
}

func (d *Dispatcher) finishReview(id, why string) {
	d.Reviews.Update(id, func(r *core.ReviewRequest) {
		r.State, r.Done, r.DoneAt, r.Updated = "done", why, now(), false
	})
	d.Log(id, map[string]string{"merged": "MR змерджено", "closed": "MR закрито", "approved": "ви схвалили MR"}[why]+": рев'ю готове")
	notify.Withdraw("review-asked:" + id)
}

func (d *Dispatcher) forgetDoneReviews(at time.Time) {
	for _, r := range d.Reviews.All() {
		if r.State != "done" {
			continue
		}
		if done, err := time.Parse(time.RFC3339Nano, r.DoneAt); err == nil && at.Sub(done) > keepDoneReviews {
			d.Reviews.Delete(r.ID)
		}
	}
}

func (d *Dispatcher) reviewLink(id string) string {
	if d.BoardURL == "" {
		return ""
	}
	return d.BoardURL + "#" + url.PathEscape(id)
}

// ReviewRequestView is an MR to review as its card shows it: CI, the open threads and the files.
type ReviewRequestView struct {
	Check *trackers.ReviewCheck `json:"check,omitempty"`
	Files []trackers.ReviewFile `json:"files"`
	Error string                `json:"error,omitempty"`
	Item  core.ReviewRequest    `json:"item"`
}

// ReviewRequest reads the MR from its code host for the card, and takes «оновлено» off it: it is seen now.
func (d *Dispatcher) ReviewRequest(ctx context.Context, id string) (*ReviewRequestView, error) {
	r, ok := d.Reviews.Get(id)
	if !ok {
		return nil, fmt.Errorf("немає такого MR на рев'ю: %s", id)
	}
	inbox, ok := d.tracker(r.TrackerID).(trackers.ReviewInbox)
	if !ok {
		return nil, fmt.Errorf("трекер %s не вміє показувати MR", r.TrackerID)
	}
	v := &ReviewRequestView{Files: []trackers.ReviewFile{}, Item: r}
	if c, err := inbox.CheckReview(ctx, r.URL); err == nil {
		v.Check = c
	} else {
		v.Error = err.Error()
	}
	if fs, err := inbox.ReviewFiles(ctx, r.URL); err == nil {
		v.Files = fs
	} else if v.Error == "" {
		v.Error = err.Error()
	}
	if r.Updated {
		v.Item = d.Reviews.Update(id, func(r *core.ReviewRequest) { r.Updated = false })
	}
	return v, nil
}
