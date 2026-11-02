package dispatch

import (
	"errors"
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
	d.agents[doing.ID] = &Agent{State: "waiting", Reason: "question"}
	if got := d.Blocked(doing); got != "the agent is waiting for you: question" {
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

// MergeReady needs a look at the MR: no look, a failed look, or anything holding the MR back is not ready.
func TestMergeReady(t *testing.T) {
	d := &Dispatcher{agents: map[string]*Agent{}, reviews: map[string]reviewLook{}}
	mr := core.Record{ID: "a#2", Status: core.Done, PRURL: "https://x/mr/2"}
	doing := core.Record{ID: "a#1", Status: core.HandedOff}
	look := func(c trackers.ReviewCheck) reviewLook { return reviewLook{at: time.Now(), check: &c} }
	green := trackers.ReviewCheck{State: "open", CI: "success", Mergeable: true}

	if d.MergeReady(mr) {
		t.Fatal("MR not looked at yet is not ready")
	}
	d.reviews[mr.ID] = reviewLook{at: time.Now(), err: errors.New("host down")}
	if d.MergeReady(mr) {
		t.Fatal("a failed look is not ready")
	}
	d.reviews[mr.ID] = look(green)
	if !d.MergeReady(mr) {
		t.Fatal("green, mergeable MR is ready")
	}
	noCI := green
	noCI.CI = ""
	d.reviews[mr.ID] = look(noCI)
	if !d.MergeReady(mr) {
		t.Fatal("an MR without CI that the host calls mergeable is ready")
	}
	for name, mod := range map[string]func(*trackers.ReviewCheck){
		"CI running":    func(c *trackers.ReviewCheck) { c.CI = "running" },
		"CI failed":     func(c *trackers.ReviewCheck) { c.CI = "failed" },
		"conflicts":     func(c *trackers.ReviewCheck) { c.Conflicts = true },
		"open thread":   func(c *trackers.ReviewCheck) { c.Threads = []trackers.Thread{{Body: "fix this"}} },
		"not mergeable": func(c *trackers.ReviewCheck) { c.Mergeable = false },
		"merged":        func(c *trackers.ReviewCheck) { c.State = "merged" },
	} {
		c := green
		mod(&c)
		d.reviews[mr.ID] = look(c)
		if d.MergeReady(mr) {
			t.Errorf("%s: must not be ready", name)
		}
	}
	d.reviews[doing.ID] = look(green)
	if d.MergeReady(doing) {
		t.Error("a task still in work is not ready")
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
