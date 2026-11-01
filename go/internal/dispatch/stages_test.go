package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func TestWorkAndMRStages(t *testing.T) {
	checking := core.Record{Checks: &core.Checks{State: "running"}}
	for _, c := range []struct {
		r         core.Record
		state     string
		reviewing bool
		want      string
	}{
		{core.Record{}, "working", false, core.StageAgent},
		{core.Record{}, "waiting", false, core.StageYou},
		{core.Record{}, "done", false, core.StageYou},
		{core.Record{}, "done", true, core.StageAgent}, // Mako's reviewer reads the branch: nobody waits for you
		{checking, "done", false, core.StageCI},
		{core.Record{}, "unknown", true, ""},
	} {
		if got := workStage(c.r, c.state, c.reviewing); got != c.want {
			t.Errorf("workStage(%s, %v) = %q, want %q", c.state, c.reviewing, got, c.want)
		}
	}
	for _, c := range []struct {
		state string
		check *trackers.ReviewCheck
		want  string
	}{
		{"working", nil, core.StageAgent},
		{"waiting", &trackers.ReviewCheck{CI: "running"}, core.StageYou},
		{"done", &trackers.ReviewCheck{CI: "running"}, core.StageCI},
		{"", &trackers.ReviewCheck{CI: "failed"}, core.StageYou},
		{"", &trackers.ReviewCheck{CI: "success"}, core.StageReview},
		{"", &trackers.ReviewCheck{}, core.StageReview},
		{"", nil, ""},
	} {
		if got := mrStage(c.state, c.check); got != c.want {
			t.Errorf("mrStage(%s, %+v) = %q, want %q", c.state, c.check, got, c.want)
		}
	}
}

func TestTrackStages(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	id := "grp/app#7"
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Executor = core.HandedOff, "orca"
		r.Task = core.Task{ID: id, TrackerID: "gitlab", Project: "grp/app"}
	})
	agent := func(state string, now time.Time) {
		r, _ := d.Store.Get(id)
		d.setAgent(r, Agent{State: state}, now)
	}
	look := func(c *trackers.ReviewCheck) {
		d.mu.Lock()
		d.reviews[id] = reviewLook{at: time.Now(), check: c}
		d.mu.Unlock()
	}
	agent("working", at)
	agent("waiting", at.Add(20*time.Minute))
	agent("working", at.Add(30*time.Minute))
	agent("unknown", at.Add(40*time.Minute)) // Orca did not answer: still working
	agent("done", at.Add(55*time.Minute))
	// Mako's reviewer agent reads the branch for 5 min before the MR: the agent's time, not yours.
	d.mu.Lock()
	d.reviewing[id] = true
	d.mu.Unlock()
	d.TrackStages(ctx, at.Add(55*time.Minute))
	d.mu.Lock()
	delete(d.reviewing, id)
	d.mu.Unlock()
	d.TrackStages(ctx, at.Add(60*time.Minute))
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PRURL = core.Done, "https://gitlab.example/grp/app/-/merge_requests/7"
	})
	look(&trackers.ReviewCheck{State: "open", CI: "running"})
	d.TrackStages(ctx, at.Add(61*time.Minute))
	look(&trackers.ReviewCheck{State: "open", CI: "success"})
	d.TrackStages(ctx, at.Add(71*time.Minute))
	d.Store.Update(id, func(r *core.Record) {
		r.ReviewState, r.MergedAt = "merged", at.Add(3*time.Hour).Format(time.RFC3339Nano)
	})
	d.TrackStages(ctx, at.Add(5*time.Hour)) // noticed later: ends at the merge
	r, _ := d.Store.Get(id)
	got := r.StageSeconds(at.Add(10 * time.Hour))
	want := map[string]float64{core.StageAgent: 50 * 60, core.StageYou: 11 * 60, core.StageCI: 10 * 60, core.StageReview: 109 * 60}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("stages %v, want %v", got, want)
		}
	}
	if r.Metrics.Stage != "" {
		t.Fatalf("still in %q", r.Metrics.Stage)
	}
}

func TestAskedCountsFixRounds(t *testing.T) {
	d, _, _ := setup(t)
	id := "grp/app#8"
	d.Store.Update(id, func(r *core.Record) { r.Status = core.Done })
	d.asked(id, "ci")
	d.asked(id, "ci")
	d.asked(id, "comments")
	d.asked(id, "conflicts")
	if r, _ := d.Store.Get(id); r.CIFixes != 2 || r.ReviewFixes != 1 {
		t.Fatalf("ci %d, comments %d", r.CIFixes, r.ReviewFixes)
	}
}
