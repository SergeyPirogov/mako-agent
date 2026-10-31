package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// «Перезапустити, коли агенти закінчать хід» waits for the agent that works, and goes once its turn ends.
func TestRestartWaitsForAgents(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	store.Update("grp/app#5", func(r *core.Record) {
		r.Status, r.Executor, r.View, r.Task = core.HandedOff, terminalExecutor, "terminal", core.Task{ID: "grp/app#5"}
	})
	d := New(&config.Config{StateDir: t.TempDir()}, store, nil)
	s, err := term.Start(term.Options{ID: "grp/app#5", Worktree: t.TempDir(), Command: "sleep", Args: []string{"30"}})
	if err != nil {
		t.Fatal(err)
	}
	d.Sessions.Put(s)
	defer s.Stop()

	if busy := d.BusyTasks(); len(busy) != 1 || busy[0] != "grp/app#5" {
		t.Fatalf("busy %v", busy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if d.WaitRestart(ctx, 10*time.Millisecond) {
		t.Fatal("restart without being asked")
	}

	d.RequestRestart(true)
	ctx, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if d.WaitRestart(ctx, 10*time.Millisecond) {
		t.Fatal("restart while the agent works")
	}

	s.Hook([]byte(`{"hook_event_name":"Stop"}`)) // its turn ended
	if busy := d.BusyTasks(); len(busy) != 0 {
		t.Fatalf("busy after Stop: %v", busy)
	}
	ctx, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	if !d.WaitRestart(ctx, 10*time.Millisecond) {
		t.Fatal("no restart once the agent is done")
	}

	d.RequestRestart(false)
	if d.RestartPending() {
		t.Fatal("restart not taken back")
	}
}
