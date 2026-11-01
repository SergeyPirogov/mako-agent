package dispatch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
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
	if got := d.Blocked(doing); got != "the agent is waiting for you: питання" {
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
	if got := d.Blocked(review); got != "CI failed" {
		t.Fatalf("failed CI: %q", got)
	}
	d.reviews[review.ID] = look(trackers.ReviewCheck{State: "open", CI: "failed", Conflicts: true})
	if got := d.Blocked(review); got != "the MR has conflicts" {
		t.Fatalf("conflicts: %q", got)
	}
	d.reviews[merged.ID] = look(trackers.ReviewCheck{State: "open", Conflicts: true})
	if got := d.Blocked(merged); got != "" {
		t.Fatalf("merged task is done, not blocked: %q", got)
	}
}

// An MR whose own agent stopped says so: “the MR has conflicts” alone reads as if the agent were on it.
func TestBlockedAgentStopped(t *testing.T) {
	d := &Dispatcher{agents: map[string]*Agent{}, reviews: map[string]reviewLook{}}
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	d.Sessions = term.NewManager(t.TempDir())
	mine := core.Record{ID: "a#2", Status: core.Done, PRURL: "https://x/mr/2", Executor: terminalExecutor, Worktree: "/wt/a-2", View: "chat"}
	orca := core.Record{ID: "a#3", Status: core.Done, PRURL: "https://x/mr/3", Worktree: "/wt/a-3"}
	look := func(c trackers.ReviewCheck) reviewLook { return reviewLook{at: time.Now(), check: &c} }

	d.reviews[mine.ID] = look(trackers.ReviewCheck{State: "open", Conflicts: true})
	if got := d.Blocked(mine); got != "agent stopped: the MR has conflicts, continue it in the chat" {
		t.Fatalf("conflicts, no agent running: %q", got)
	}
	d.reviews[mine.ID] = look(trackers.ReviewCheck{State: "open", CI: "failed"})
	if got := d.Blocked(mine); got != "agent stopped: CI failed, continue it in the chat" {
		t.Fatalf("failed CI, no agent running: %q", got)
	}
	d.reviews[mine.ID] = look(trackers.ReviewCheck{State: "open", CI: "success"})
	if got := d.Blocked(mine); got != "" {
		t.Fatalf("green MR needs no agent: %q", got)
	}
	d.reviews[orca.ID] = look(trackers.ReviewCheck{State: "open", Conflicts: true})
	if got := d.Blocked(orca); got != "the MR has conflicts" {
		t.Fatalf("an agent in Orca is not Mako's to look at: %q", got)
	}
}
