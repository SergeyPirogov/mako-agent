package dispatch

import (
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func TestBlocked(t *testing.T) {
	d := &Dispatcher{agents: map[string]*Agent{}, reviews: map[string]reviewLook{}}
	doing := core.Record{ID: "a#1", Status: core.HandedOff}
	review := core.Record{ID: "a#2", Status: core.Done, PRURL: "https://x/mr/2"}
	merged := core.Record{ID: "a#3", Status: core.Done, PRURL: "https://x/mr/3", ReviewState: "merged"}

	if got := d.Blocked(doing); got != "" {
		t.Fatalf("no agent look yet: %q", got)
	}
	d.agents[doing.ID] = &Agent{State: "working"}
	if got := d.Blocked(doing); got != "" {
		t.Fatalf("working agent: %q", got)
	}
	d.agents[doing.ID] = &Agent{State: "waiting", Reason: "питання"}
	if got := d.Blocked(doing); got != "агент чекає на вас: питання" {
		t.Fatalf("waiting agent: %q", got)
	}

	look := func(c trackers.ReviewCheck) reviewLook { return reviewLook{at: time.Now(), check: &c} }
	if got := d.Blocked(review); got != "" {
		t.Fatalf("MR not looked at: %q", got)
	}
	d.reviews[review.ID] = look(trackers.ReviewCheck{State: "open", CI: "success"})
	if got := d.Blocked(review); got != "" {
		t.Fatalf("green MR: %q", got)
	}
	d.reviews[review.ID] = look(trackers.ReviewCheck{State: "open", CI: "failed"})
	if got := d.Blocked(review); got != "CI не пройшов" {
		t.Fatalf("failed CI: %q", got)
	}
	d.reviews[review.ID] = look(trackers.ReviewCheck{State: "open", CI: "failed", Conflicts: true})
	if got := d.Blocked(review); got != "конфлікти в MR" {
		t.Fatalf("conflicts: %q", got)
	}
	d.reviews[merged.ID] = look(trackers.ReviewCheck{State: "open", Conflicts: true})
	if got := d.Blocked(merged); got != "" {
		t.Fatalf("merged task is done, not blocked: %q", got)
	}
}
