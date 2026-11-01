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

// reviewsEvery: how often the MRs to review are searched; less often than the tasks, for the code hosts'
// limits (GitHub's search allows 30 requests a minute).
const reviewsEvery = 3 * time.Minute

// pollReviews looks for the tracker's MRs waiting for my review: new ones are told about once, new commits
// mark the card «оновлено», and one merged, closed or approved by me goes to «Готово».
func (d *Dispatcher) pollReviews(ctx context.Context, t trackers.Tracker) {
	inbox, ok := t.(trackers.ReviewInbox)
	if !ok || !t.Config().ReviewRequests || d.Reviews == nil {
		return
	}
	d.mu.Lock()
	if last, ok := d.reviewsAt[t.ID()]; ok && time.Since(last) < d.reviewsEvery {
		d.mu.Unlock()
		return
	}
	d.reviewsAt[t.ID()] = time.Now()
	d.mu.Unlock()
	found, err := inbox.ToReview(ctx)
	if err != nil {
		d.Fault(t.ID(), "пошук MR на рев'ю не вдався", err)
		return
	}
	// The first search of a tracker (a new Mako, the switch just turned on) only fills the list: no
	// notifications about MRs that have been waiting for long, and they wait since they were opened.
	first := true
	for _, r := range d.Reviews.All() {
		if r.TrackerID == t.ID() {
			first = false
			break
		}
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
		// A request of the poll that failed leaves a value empty: what was known of it stays.
		if known {
			if f.HeadSHA == "" {
				f.HeadSHA = prev.HeadSHA
			}
			if f.HeadSHA == prev.HeadSHA {
				if f.CI == "" {
					f.CI = prev.CI
				}
				if f.Additions == 0 && f.Deletions == 0 {
					f.Additions, f.Deletions = prev.Additions, prev.Deletions
				}
			}
		}
		since := prev.Since
		switch {
		case !known && first && f.Since != "":
			since = f.Since
		case !known || reopened:
			since = stamp
		}
		d.Reviews.Update(f.ID, func(r *core.ReviewRequest) {
			// New commits since it was last seen; a review asked again after it was done is news too.
			updated := r.Updated || known && (reopened || prev.HeadSHA != "" && f.HeadSHA != prev.HeadSHA)
			*r = f
			r.Since, r.Updated, r.State = since, updated, "open"
		})
		if !known {
			d.Log(f.ID, "вас призначили рев'ювером: "+f.Title)
		}
		if !known && !first {
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

// dropReviews forgets the tracker's MRs to review: «Рев'ю для мене» is turned off for it.
func (d *Dispatcher) dropReviews(trackerID string) {
	if d.Reviews == nil {
		return
	}
	for _, r := range d.Reviews.All() {
		if r.TrackerID == trackerID {
			d.Reviews.Delete(r.ID)
		}
	}
}

// ReviewList: the MRs to review the board shows, of the trackers that have them turned on.
func (d *Dispatcher) ReviewList() []core.ReviewRequest {
	out := []core.ReviewRequest{}
	if d.Reviews == nil {
		return out
	}
	on := map[string]bool{}
	for _, t := range d.TrackerList() {
		if _, ok := t.(trackers.ReviewInbox); ok && t.Config().ReviewRequests {
			on[t.ID()] = true
		}
	}
	for _, r := range d.Reviews.All() {
		if on[r.TrackerID] {
			out = append(out, r)
		}
	}
	return out
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
	if d.Reviews == nil {
		return nil, fmt.Errorf("немає такого MR на рев'ю: %s", id)
	}
	r, ok := d.Reviews.Get(id)
	if !ok {
		return nil, fmt.Errorf("немає такого MR на рев'ю: %s", id)
	}
	t := d.tracker(r.TrackerID)
	inbox, ok := t.(trackers.ReviewInbox)
	if !ok || !t.Config().ReviewRequests {
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
