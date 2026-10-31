package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestRetro(t *testing.T) {
	d, _, _ := setup(t)
	id := "grp/app#7"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PRURL, r.Attempt, r.AutoFixes = core.Done, "u", 2, 1
		r.StartedAt, r.FinishedAt, r.MergedAt = "2026-10-01T10:00:00Z", "2026-10-01T11:30:00Z", "2026-10-02T09:00:00Z"
		r.RetryNote = "Не чіпай API"
		r.AgentReviews = []core.AgentReview{{State: "notes", Notes: "- немає тесту"}, {State: "clean"}}
	})
	if err := d.Retro(id); err == nil {
		t.Fatal("a retro for a task not merged yet")
	}
	d.Store.Update(id, func(r *core.Record) { r.ReviewState = "merged" })
	story, _ := d.Store.Get(id)
	for _, want := range []string{"attempts: 2", "1h30m0s", "21h30m0s", "sent back to fix its merge request (CI, conflicts, review comments): 1", "Не чіпай API", "round 1, found:\n- немає тесту", "round 2: nothing to fix"} {
		if s := d.story(story); !strings.Contains(s, want) {
			t.Errorf("no %q in the story:\n%s", want, s)
		}
	}
	// No Claude Code in tests: the retro fails, and says so on the card.
	if err := d.startRetro(id); err != nil {
		t.Fatal(err)
	}
	if err := d.startRetro(id); err == nil {
		t.Fatal("a second retro while one runs")
	}
	d.retro(context.Background(), id)
	if r, _ := d.Store.Get(id); r.Retro == nil || r.Retro.State != "failed" || r.Retro.Error == "" {
		t.Fatalf("retro %+v", r.Retro)
	}
}
