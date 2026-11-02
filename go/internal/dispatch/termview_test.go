package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// The view switched in the middle of a turn waits for the turn to end; to the view it is in, it is cancelled.
func TestSwitchViewWaitsForTheTurn(t *testing.T) {
	d, _, _ := setup(t)
	wt := t.TempDir()
	d.Store.Update("t#1", func(r *core.Record) { r.Worktree, r.Executor, r.Status = wt, terminalExecutor, core.HandedOff })
	s, err := term.Start(term.Options{ID: "t#1", Worktree: wt, Command: "/bin/sh", Args: []string{"-c", "sleep 30"},
		OnState: func() { d.watchTerminals(time.Now()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	d.Sessions.Put(s)

	if err := d.SwitchView("t#1", "chat", false); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get("t#1"); r.ViewNext != "chat" || r.View != "" || !s.Running() {
		t.Fatalf("view %q, next %q, running %v", r.View, r.ViewNext, s.Running())
	}
	// «Cancel»: back to the view it is in.
	d.SwitchView("t#1", "terminal", false)
	if r, _ := d.Store.Get("t#1"); r.ViewNext != "" {
		t.Fatalf("not cancelled: next %q", r.ViewNext)
	}

	// Asked again, then the turn ends: the agent moves.
	d.SwitchView("t#1", "chat", false)
	s.Hook([]byte(`{"hook_event_name":"Stop"}`))
	waitFor(t, func() bool { r, _ := d.Store.Get("t#1"); return r.View == "chat" && r.ViewNext == "" }, "the switch did not follow the turn's end")
	if s.Running() {
		t.Fatal("the terminal's agent still runs after the switch")
	}
}

// The bar over the terminal: the hooks' state, and from the transcript the turn's steps, cost and the agent's plan.
func TestTerminalStatus(t *testing.T) {
	d, _, _ := setup(t)
	conf, wt := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "s-1.jsonl"), []byte(`{"type":"user","message":{"content":"Go"}}
{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-1","usage":{"input_tokens":1000000,"output_tokens":0},"content":[{"type":"tool_use","id":"t1","name":"TodoWrite","input":{"todos":[{"content":"A","status":"completed"},{"content":"B","status":"in_progress","activeForm":"Doing B"}]}}]}}
`), 0o600)
	d.Store.Update("t#1", func(r *core.Record) {
		r.Worktree, r.Executor, r.SessionID = wt, terminalExecutor, "s-1"
		r.AgentRoute = &core.Route{Model: "opus", Effort: "high"}
	})
	st, ok := d.TerminalStatus("t#1")
	if !ok || st.Progress == nil || st.Progress.Done != 1 || st.Progress.Total != 2 || st.Turn.Steps != 1 || st.Effort != "high" {
		t.Fatalf("status %+v", st)
	}
	if want := core.DefaultPrices().Cost("claude-opus-4-1", core.Usage{Input: 1000000}); want == 0 || st.Turn.Cost != want || !st.Turn.Estimate {
		t.Fatalf("turn cost %v (estimate %v), want %v", st.Turn.Cost, st.Turn.Estimate, want)
	}
	if st.Context != 1000000 || st.Window != 1000000 || st.Live {
		t.Fatalf("context %d of %d, live %v", st.Context, st.Window, st.Live)
	}
	if _, ok := d.TerminalStatus("nope"); ok {
		t.Fatal("a task not in Mako has a status")
	}
}

// Terminal.app gets one shell line: quotes in the path and the session stay quoted.
func TestTerminalScript(t *testing.T) {
	if got := terminalScript("/w/it's here", "/bin/claude", "abc-1"); got != `cd '/w/it'\''s here' && '/bin/claude' --resume 'abc-1'` {
		t.Fatalf("script %s", got)
	}
	if got := terminalScript("/w", "claude", ""); got != `cd '/w' && 'claude' --continue` {
		t.Fatalf("script %s", got)
	}
	if got := appleString(`a "b" \c`); got != `"a \"b\" \\c"` {
		t.Fatalf("apple %s", got)
	}
}
