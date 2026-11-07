package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
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

// A retro that failed says on its card, shortly, why and what to do; Claude's own words stay in the journal. A limit
// or a login of the Claude account is not Mako's error, any other failure is.
func TestRetroFailureCard(t *testing.T) {
	for name, tc := range map[string]struct {
		body, card, journal string
		fault               bool
	}{
		"limit":  {`echo '{"type":"result","is_error":true,"result":"You'"'"'ve hit your session limit"}'; exit 1`, "limit is reached", "hit your session limit", false},
		"login":  {`echo "Please run /login" >&2; exit 1`, "not logged in", "Please run /login", false},
		"other":  {`echo "boom: bad flag" >&2; exit 1`, "Claude failed to answer", "boom: bad flag", true},
		"silent": {`exit 1`, "Claude failed to answer", "no output", true},
	} {
		t.Run(name, func(t *testing.T) {
			d, _, _ := setup(t)
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+tc.body+"\n"), 0o755)
			writer.Claude = filepath.Join(dir, "claude") // setup restores it
			id := "grp/app#7"
			d.Store.Update(id, func(r *core.Record) { r.ReviewState = "merged" })
			if err := d.startRetro(id); err != nil {
				t.Fatal(err)
			}
			d.retro(context.Background(), id)
			r, _ := d.Store.Get(id)
			x := r.Retro
			if x == nil || x.State != "failed" || !strings.HasPrefix(x.Error, "The retro was not written: ") || !strings.Contains(x.Error, tc.card) ||
				strings.Contains(x.Error, "exit status") || !strings.Contains(x.Error, "ry again") {
				t.Fatalf("card %+v", x)
			}
			found := false
			for _, l := range d.Logs(id) {
				found = found || strings.Contains(l.Text, tc.journal)
			}
			if !found {
				t.Errorf("the journal lacks %q: %+v", tc.journal, d.Logs(id))
			}
			if got := len(d.Faults()) > 0; got != tc.fault {
				t.Errorf("kept among Mako's errors = %v, want %v: %+v", got, tc.fault, d.Faults())
			}
		})
	}
}

func TestRetroFailureOtherError(t *testing.T) {
	if got := retroFailure(errors.New("the retro is empty")); got != "The retro was not written: the retro is empty. Try again." {
		t.Fatalf("%q", got)
	}
}
