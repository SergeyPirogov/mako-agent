package dispatch

import (
	"context"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// ciGitLab: the test's GitLab whose MR check says what the test wants.
type ciGitLab struct {
	*trackers.GitLab
	check trackers.ReviewCheck
}

func (f *ciGitLab) CheckReview(context.Context, string) (*trackers.ReviewCheck, error) {
	c := f.check
	return &c, nil
}

// The MR tab's fresh look wakes the loop once the task waits for CI that has passed: the task moves on at once.
func TestReviewWakesOnGreenCI(t *testing.T) {
	d, _, _ := setup(t)
	f := &ciGitLab{GitLab: d.TrackerList()[0].(*trackers.GitLab)}
	d.Trackers = []trackers.Tracker{f}
	id := "grp/app#5"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PRURL = core.HandedOff, "https://gitlab.example/grp/app/-/merge_requests/1"
		r.Task = core.Task{ID: id, TrackerID: f.ID(), Project: "grp/app", Title: "Кнопка"}
	})
	woken := func() bool {
		select {
		case <-d.wake:
			return true
		default:
			return false
		}
	}
	for _, c := range []struct {
		name  string
		check trackers.ReviewCheck
		want  bool
	}{
		{"running", trackers.ReviewCheck{State: "open", CI: "running"}, false},
		{"pending", trackers.ReviewCheck{State: "open", CI: "pending"}, false},
		{"merged", trackers.ReviewCheck{State: "merged", CI: "success"}, false},
		{"green", trackers.ReviewCheck{State: "open", CI: "success"}, true},
		{"failed", trackers.ReviewCheck{State: "open", CI: "failed"}, true},
	} {
		d.forgetReview(id)
		f.check = c.check
		if _, err := d.Review(context.Background(), id); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := woken(); got != c.want {
			t.Fatalf("%s: woken %v, want %v", c.name, got, c.want)
		}
	}

	// A task already in review needs no wake-up.
	d.Store.Update(id, func(r *core.Record) { r.Status = core.Done })
	d.forgetReview(id)
	f.check = trackers.ReviewCheck{State: "open", CI: "success"}
	d.Review(context.Background(), id)
	if woken() {
		t.Fatal("a task that is not handed off woke the loop")
	}
}
