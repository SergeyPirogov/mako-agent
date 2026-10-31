package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

func TestTerminalHandOff(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	// A stand-in for claude: it writes down how it was started and stays, as an agent waits for input.
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\nexec sleep 30\n"), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	d.BoardURL = "http://127.0.0.1:1"

	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.HandedOff || r.Executor != "terminal" || len(*jobs) != 0 {
		t.Fatalf("status %s, executor %q, error %q, orca jobs %d", r.Status, r.Executor, r.Error, len(*jobs))
	}
	if !strings.HasSuffix(r.Worktree, filepath.Join(".worktrees", "app", "app-5")) || !exists(r.Worktree) {
		t.Fatalf("worktree %q", r.Worktree)
	}
	git(t, r.Worktree, "rev-parse", "--verify", "refs/heads/"+r.Branch)
	s := d.Sessions.Get(r.ID)
	if s == nil || !s.Running() {
		t.Fatal("no agent running")
	}
	defer d.StopTerminal(r.ID)
	var args string
	for i := 0; i < 100 && args == ""; i++ {
		b, _ := os.ReadFile(argsFile)
		args = string(b)
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(args, "--settings\n") || !strings.Contains(args, "Кнопка оплати не працює") ||
		!strings.HasSuffix(args, "--permission-mode\nauto\n") {
		t.Fatalf("args %q", args)
	}
	if len(f.notes) != 1 {
		t.Fatalf("start comment: %v", f.notes)
	}

	// The hooks move the card: the agent waits for you, then finishes its turn.
	d.watchTerminals(time.Now())
	if a := d.Agents()[r.ID]; a.State != "working" || !a.Terminal || !a.Live {
		t.Fatalf("agent %+v", a)
	}
	s.Hook([]byte(`{"hook_event_name":"Stop"}`))
	if a := d.Agents()[r.ID]; a.State != "done" {
		t.Fatalf("after Stop: %+v", a)
	}

	// Cancel stops the agent, the worktree stays.
	d.Cancel(r.ID)
	for i := 0; i < 200 && s.Running(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Running() || !exists(r.Worktree) {
		t.Fatalf("running %v, worktree %v", s.Running(), exists(r.Worktree))
	}
}
