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

func askSetup(t *testing.T) (*Dispatcher, *inboxTracker, *int) {
	d, _, _ := setup(t)
	f := &inboxTracker{Tracker: d.TrackerList()[0], states: map[string]string{}}
	d.Trackers = []trackers.Tracker{f}
	d.reviewsEvery = 0 // every poll searches
	told := 0
	notify.Muted = func(kind string) bool {
		if kind == notify.ReviewAsked {
			told++
		}
		return true
	}
	t.Cleanup(func() { notify.Muted = nil })
	return d, f, &told
}

// An MR where I am a reviewer: on the board once, told about once, «оновлено» after new commits, and in
// «Готово» once merged, closed or approved by me; one I was taken off goes away.
func TestReviewRequests(t *testing.T) {
	d, f, told := askSetup(t)
	ctx := context.Background()

	a, b, c := mrToReview(1, "aaa"), mrToReview(2, "bbb"), mrToReview(3, "ccc")
	a.Since = "2026-01-02T03:04:05Z"          // opened long ago
	f.open = []core.ReviewRequest{a, b, c, a} // GitHub's two searches may find one PR twice
	d.Poll(ctx)
	d.Poll(ctx)
	if all := d.Reviews.All(); len(all) != 3 || *told != 0 {
		t.Fatalf("the first search fills the list quietly: %d MRs, told %d", len(all), *told)
	}
	if r, _ := d.Reviews.Get(a.ID); r.State != "open" || r.Since != a.Since || r.Updated || r.Additions != 3 {
		t.Fatalf("new MR %+v", r)
	}

	// A new MR after that is told about, once, and waits since Mako saw it.
	e := mrToReview(5, "eee")
	e.Since = "2026-01-02T03:04:05Z"
	f.open = append(f.open, e)
	d.Poll(ctx)
	d.Poll(ctx)
	if r, _ := d.Reviews.Get(e.ID); *told != 1 || r.Since == e.Since {
		t.Fatalf("told %d, since %s", *told, r.Since)
	}

	// A failed request leaves values empty: what was known stays.
	broken := a
	broken.HeadSHA, broken.CI, broken.Additions, broken.Deletions = "", "", 0, 0
	f.open = []core.ReviewRequest{broken, b, c, e}
	d.Poll(ctx)
	if r, _ := d.Reviews.Get(a.ID); r.HeadSHA != "aaa" || r.Additions != 3 || r.Updated {
		t.Fatalf("kept %+v", r)
	}

	// New commits in a; b approved by me; c merged and e closed, so they leave the search.
	a.HeadSHA = "aa2"
	b.Approved = true
	f.open = []core.ReviewRequest{a, b}
	f.states[c.URL], f.states[e.URL] = "merged", "closed"
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
	if r, _ := d.Reviews.Get(e.ID); r.State != "done" || r.Done != "closed" {
		t.Fatalf("closed MR %+v", r)
	}

	// Opening the card takes «оновлено» off.
	v, err := d.ReviewRequest(ctx, a.ID)
	if err != nil || v.Item.Updated || len(v.Files) != 1 || v.Check.CI != "success" {
		t.Fatalf("card %+v %v", v, err)
	}

	// Review asked again of b after my approval: open again, «оновлено», waiting from now.
	before, _ := d.Reviews.Get(b.ID)
	b.Approved = false
	f.open = []core.ReviewRequest{a, b}
	d.Poll(ctx)
	if r, _ := d.Reviews.Get(b.ID); r.State != "open" || !r.Updated || r.Done != "" || r.Since <= before.Since {
		t.Fatalf("asked again %+v (was %+v)", r, before)
	}

	// Taken off as a reviewer: still open, but no longer mine to review.
	f.open = []core.ReviewRequest{b}
	f.states[a.URL] = "open"
	d.Poll(ctx)
	if _, ok := d.Reviews.Get(a.ID); ok {
		t.Fatal("an MR I no longer review stays")
	}
	if *told != 1 {
		t.Fatalf("told again: %d", *told)
	}

	// A week later the finished ones are forgotten.
	d.forgetDoneReviews(time.Now().Add(8 * 24 * time.Hour))
	if n := len(d.Reviews.All()); n != 1 {
		t.Fatalf("done MRs kept: %d", n)
	}
}

// Turned off for the tracker: no search, its cards are gone from the board and the store; turned on again,
// the first search is quiet again.
func TestReviewRequestsOff(t *testing.T) {
	d, f, told := askSetup(t)
	ctx := context.Background()
	f.open = []core.ReviewRequest{mrToReview(1, "a")}
	d.Poll(ctx)
	if len(d.ReviewList()) != 1 {
		t.Fatal("not on the board")
	}
	f.Config().ReviewRequests = false
	if len(d.ReviewList()) != 0 {
		t.Fatal("the board shows it while turned off")
	}
	f.open = append(f.open, mrToReview(2, "b"))
	d.Poll(ctx)
	if n := len(d.Reviews.All()); n != 0 {
		t.Fatalf("kept or searched while off: %d", n)
	}
	f.Config().ReviewRequests = true
	d.Poll(ctx)
	if n := len(d.Reviews.All()); n != 2 || *told != 0 {
		t.Fatalf("turned on: %d MRs, told %d", n, *told)
	}
	// A tracker gone from the settings: its cards are not shown, and not opened.
	d.Trackers = nil
	if len(d.ReviewList()) != 0 {
		t.Fatal("cards of a removed tracker shown")
	}
	if _, err := d.ReviewRequest(ctx, mrToReview(1, "a").ID); err == nil {
		t.Fatal("card of a removed tracker opened")
	}
}

// A tracker on pause is not searched for MRs to review either; and not more often than reviewsEvery.
func TestReviewRequestsPausedAndThrottled(t *testing.T) {
	d, f, _ := askSetup(t)
	ctx := context.Background()
	f.open = []core.ReviewRequest{mrToReview(1, "a")}
	f.Config().Paused = true
	d.Poll(ctx)
	if n := len(d.Reviews.All()); n != 0 {
		t.Fatalf("searched on pause: %d", n)
	}
	f.Config().Paused = false
	d.reviewsEvery = time.Hour
	d.Poll(ctx)
	f.open = append(f.open, mrToReview(2, "b"))
	d.Poll(ctx)
	if n := len(d.Reviews.All()); n != 1 {
		t.Fatalf("searched again within reviewsEvery: %d", n)
	}
}
