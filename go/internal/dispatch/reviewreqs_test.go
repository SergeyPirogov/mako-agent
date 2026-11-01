package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// inboxTracker: the test's GitLab with the MRs to review given by the test.
type inboxTracker struct {
	trackers.Tracker
	open   []core.ReviewRequest
	states map[string]string
}

func (f *inboxTracker) ToReview(context.Context) ([]core.ReviewRequest, error) { return f.open, nil }
func (f *inboxTracker) ReviewState(_ context.Context, url string) (string, error) {
	return f.states[url], nil
}
func (f *inboxTracker) ReviewFiles(context.Context, string) ([]trackers.ReviewFile, error) {
	return []trackers.ReviewFile{{Path: "a.go", Status: "M", Diff: "@@ -1 +1 @@\n-a\n+b"}}, nil
}
func (f *inboxTracker) CheckReview(context.Context, string) (*trackers.ReviewCheck, error) {
	return &trackers.ReviewCheck{State: "open", CI: "success"}, nil
}

func mrToReview(n int, sha string) core.ReviewRequest {
	url := "https://git.example.com/grp/lib/-/merge_requests/" + string(rune('0'+n))
	return core.ReviewRequest{ID: url, URL: url, TrackerID: "gitlab", Project: "grp/lib", Number: n, Ref: "!" + string(rune('0'+n)),
		Title: "Зміна", Author: "ann", HeadSHA: sha, Additions: 3, Deletions: 1}
}

// An MR where I am a reviewer: on the board once, told about once, «оновлено» after new commits, and in
// «Готово» once merged or approved by me; one I was taken off goes away.
func TestReviewRequests(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	f := &inboxTracker{Tracker: d.TrackerList()[0], states: map[string]string{}}
	d.Trackers = []trackers.Tracker{f}
	told := 0
	notify.Muted = func(kind string) bool {
		if kind == notify.ReviewAsked {
			told++
		}
		return true
	}
	t.Cleanup(func() { notify.Muted = nil })

	a, b, c := mrToReview(1, "aaa"), mrToReview(2, "bbb"), mrToReview(3, "ccc")
	f.open = []core.ReviewRequest{a, b, c, a} // GitHub's two searches may find one PR twice
	d.Poll(ctx)
	d.Poll(ctx)
	if all := d.Reviews.All(); len(all) != 3 || told != 3 {
		t.Fatalf("want 3 MRs told about once each, got %d told %d: %+v", len(all), told, all)
	}
	if r, _ := d.Reviews.Get(a.ID); r.State != "open" || r.Since == "" || r.Updated || r.Additions != 3 {
		t.Fatalf("new MR %+v", r)
	}

	// New commits in a; b approved by me; c merged, so it leaves the search.
	a.HeadSHA = "aa2"
	b.Approved = true
	f.open = []core.ReviewRequest{a, b}
	f.states[c.URL] = "merged"
	d.Poll(ctx)
	if r, _ := d.Reviews.Get(a.ID); !r.Updated || r.State != "open" {
		t.Fatalf("updated MR %+v", r)
	}
	if r, _ := d.Reviews.Get(b.ID); r.State != "done" || r.Done != "approved" {
		t.Fatalf("approved MR %+v", r)
	}
	if r, _ := d.Reviews.Get(c.ID); r.State != "done" || r.Done != "merged" || r.DoneAt == "" {
		t.Fatalf("merged MR %+v", r)
	}

	// Opening the card takes «оновлено» off.
	v, err := d.ReviewRequest(ctx, a.ID)
	if err != nil || v.Item.Updated || len(v.Files) != 1 || v.Check.CI != "success" {
		t.Fatalf("card %+v %v", v, err)
	}

	// Taken off as a reviewer: still open, but no longer mine to review.
	f.open = nil
	f.states[a.URL] = "open"
	d.Poll(ctx)
	if _, ok := d.Reviews.Get(a.ID); ok {
		t.Fatal("an MR I no longer review stays")
	}
	if told != 3 {
		t.Fatalf("told again: %d", told)
	}

	// A week later the finished ones are forgotten.
	d.forgetDoneReviews(time.Now().Add(8 * 24 * time.Hour))
	if n := len(d.Reviews.All()); n != 0 {
		t.Fatalf("done MRs kept: %d", n)
	}
}

// Turned off for the tracker: no MRs to review are searched.
func TestReviewRequestsOff(t *testing.T) {
	d, _, _ := setup(t)
	f := &inboxTracker{Tracker: d.TrackerList()[0], open: []core.ReviewRequest{mrToReview(1, "a")}}
	d.Trackers = []trackers.Tracker{f}
	f.Config().ReviewRequests = false
	d.Poll(context.Background())
	if n := len(d.Reviews.All()); n != 0 {
		t.Fatalf("searched while off: %d", n)
	}
}
