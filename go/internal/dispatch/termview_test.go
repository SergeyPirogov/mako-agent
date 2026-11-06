package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
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

// chatInTheChat: a task whose agent runs in the chat on a fake claude with a conversation to resume. The fake notes
// its arguments, and then ends its turn (turn) or goes on with it for ever. Its next start notes them again.
func chatInTheChat(t *testing.T, turn bool) (d *Dispatcher, wt string, s *chat.Session) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	d, _, _ = setup(t)
	wt = t.TempDir()
	bin := t.TempDir()
	result := ""
	if turn {
		result = `echo '{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.01}'`
	}
	script := "#!/bin/sh\necho \"$@\" > args.txt\nread init\nread msg\necho \"$msg\" > prompt.json\n" +
		`echo '{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5-5"}'` + "\n" + result + "\ncat > /dev/null\n"
	os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755)
	cfg := *d.Config()
	cfg.Terminal.Command, cfg.StateDir = filepath.Join(bin, "claude"), t.TempDir()
	d.Cfg = &cfg
	projects := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(projects, 0o755)
	os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}`+"\n"), 0o644)
	d.Store.Update("t#1", func(r *core.Record) {
		r.Worktree, r.Executor, r.Status, r.View, r.SessionID = wt, terminalExecutor, core.HandedOff, "chat", "sess-1"
	})
	s = chat.Start(chat.Options{ID: "t#1", Worktree: wt, Command: cfg.Terminal.Command, Prompt: "work", OnState: func() { d.watchTerminals(time.Now()) }})
	d.Chats.Put(s)
	want := "working"
	if turn {
		want = "done"
	}
	waitFor(t, func() bool { st, _, _ := s.State(); return st == want }, "the chat is not "+want)
	os.Remove(filepath.Join(wt, "args.txt"))
	t.Cleanup(func() {
		d.StopTerminal("t#1")
		s.Wait()
		if ts := d.Sessions.Get("t#1"); ts != nil {
			ts.Stop()
			ts.Wait()
		}
		d.WaitContexts()
	})
	return d, wt, s
}

func agentArgs(wt string) string {
	b, _ := os.ReadFile(filepath.Join(wt, "args.txt"))
	return string(b)
}

// An agent that has ended its turn is switched at once, and nothing is said to it: nothing was interrupted.
func TestSwitchViewOfAnIdleAgent(t *testing.T) {
	d, wt, s := chatInTheChat(t, true)
	if err := d.SwitchView("t#1", "terminal", false); err != nil {
		t.Fatal(err)
	}
	r, _ := d.Store.Get("t#1")
	if r.View != "terminal" || r.ViewNext != "" || s.Running() {
		t.Fatalf("view %q, next %q, the chat runs %v", r.View, r.ViewNext, s.Running())
	}
	waitFor(t, func() bool { return agentArgs(wt) != "" }, "the agent did not start in the terminal")
	if args := agentArgs(wt); !strings.Contains(args, "--resume sess-1") || strings.Contains(args, i18n.TA("agent.switched")) {
		t.Fatalf("args %s", args)
	}
}

// «Switch now» in the middle of a turn: the conversation goes on in the other view with a word that it was cut, not
// from an empty prompt.
func TestSwitchViewNowTellsTheAgent(t *testing.T) {
	d, wt, s := chatInTheChat(t, false)
	if err := d.SwitchView("t#1", "terminal", true); err != nil {
		t.Fatal(err)
	}
	r, _ := d.Store.Get("t#1")
	if r.View != "terminal" || r.ViewNext != "" || s.Running() {
		t.Fatalf("view %q, next %q, the chat runs %v", r.View, r.ViewNext, s.Running())
	}
	waitFor(t, func() bool { return agentArgs(wt) != "" }, "the agent did not start in the terminal")
	args := agentArgs(wt)
	if !strings.Contains(args, "--resume sess-1") || !strings.Contains(args, i18n.TA("agent.switched")) {
		t.Fatalf("args %s", args)
	}
}

// «Switch now» while the agent waits for your answer: it is told its question was lost, not to go on without it.
func TestSwitchViewNowWhileAsking(t *testing.T) {
	d, wt, s := chatInTheChat(t, false)
	// The agent is in the terminal, asking for a permission.
	d.StopTerminal("t#1")
	s.Wait()
	d.Store.Update("t#1", func(r *core.Record) { r.View = "" })
	ts, err := term.Start(term.Options{ID: "t#1", Worktree: wt, Command: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	d.Sessions.Put(ts)
	ts.Hook([]byte(`{"hook_event_name":"Notification","message":"Claude needs your permission"}`))
	if !d.agentWaiting("t#1") || d.agentIdle("t#1") {
		t.Fatal("the agent is not waiting")
	}
	if err := d.SwitchView("t#1", "chat", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(wt, "prompt.json"))
		return strings.HasSuffix(string(b), "\n")
	}, "the agent got no message in the chat")
	b, _ := os.ReadFile(filepath.Join(wt, "prompt.json"))
	if !strings.Contains(string(b), "Постав його ще раз") || strings.Contains(string(b), "продовж з того місця") {
		t.Fatalf("message %s", b)
	}
}

// The switch that was due when the turn ended but failed stays asked: it is not dropped.
func TestSwitchWhenIdleKeepsTheSwitchOnFailure(t *testing.T) {
	old := closeWait
	closeWait = 100 * time.Millisecond
	defer func() { closeWait = old }()
	d, _, _ := setup(t)
	wt := t.TempDir()
	d.Store.Update("t#1", func(r *core.Record) {
		r.Worktree, r.Executor, r.Status, r.ViewNext = wt, terminalExecutor, core.HandedOff, "chat"
	})
	s, err := term.Start(term.Options{ID: "t#1", Worktree: wt, Command: "/bin/sh", Args: []string{"-c", "trap '' HUP; touch trapped; sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Stop(); s.Wait() }()
	d.Sessions.Put(s)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(wt, "trapped")); return err == nil }, "the agent does not ignore the hangup yet")
	s.Hook([]byte(`{"hook_event_name":"Stop"}`))
	d.switchWhenIdle("t#1")
	if r, _ := d.Store.Get("t#1"); r.View != "" || r.ViewNext != "chat" {
		t.Fatalf("view %q, next %q: the switch was lost", r.View, r.ViewNext)
	}
}

// An agent that does not end in time is an error, not a silent no-op: the view stays and so does the switch asked for.
func TestSwitchViewOldAgentStillClosing(t *testing.T) {
	old := closeWait
	closeWait = 200 * time.Millisecond
	defer func() { closeWait = old }()
	d, _, _ := setup(t)
	wt := t.TempDir()
	d.Store.Update("t#1", func(r *core.Record) {
		r.Worktree, r.Executor, r.Status, r.ViewNext = wt, terminalExecutor, core.HandedOff, "chat"
	})
	// It ignores the hangup that stops it: only the kill after the grace period ends it.
	s, err := term.Start(term.Options{ID: "t#1", Worktree: wt, Command: "/bin/sh", Args: []string{"-c", "trap '' HUP; touch trapped; sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Stop(); s.Wait() }()
	d.Sessions.Put(s)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(wt, "trapped")); return err == nil }, "the agent does not ignore the hangup yet")
	err = d.SwitchView("t#1", "chat", true)
	if err == nil || !strings.Contains(err.Error(), "still closing") {
		t.Fatalf("error %v", err)
	}
	if r, _ := d.Store.Get("t#1"); r.View != "" || r.ViewNext != "chat" {
		t.Fatalf("view %q, next %q", r.View, r.ViewNext)
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
