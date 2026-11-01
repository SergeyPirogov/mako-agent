package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
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
	cfg.Terminal.SetupCommand = "echo ready > prepared.txt"
	cfg.Terminal.View = "terminal"
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	d.BoardURL = "http://127.0.0.1:1"

	d.Poll(ctx)
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Model = "haiku" }) // picked on the card
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.AgentModel != "haiku" { // the terminal does not report its model: the one Mako asked for
		t.Fatalf("agent model %q", r.AgentModel)
	}
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
	// The stand-in may still be writing it.
	waitFor(t, func() bool {
		b, _ := os.ReadFile(argsFile)
		args = string(b)
		return strings.HasSuffix(args, "haiku\n")
	}, "the agent's args were not written")
	if !strings.HasPrefix(args, "--settings\n") || !strings.Contains(args, "Кнопка оплати не працює") ||
		!strings.HasSuffix(args, "--permission-mode\nauto\n--model\nhaiku\n") {
		t.Fatalf("args %q", args)
	}
	// The setup ran in the worktree before the agent.
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "prepared.txt")); string(b) != "ready\n" {
		t.Fatalf("setup: %q", b)
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
	waitFor(t, func() bool { return !s.Running() }, "the agent did not stop")
	if s.Running() || !exists(r.Worktree) {
		t.Fatalf("running %v, worktree %v", s.Running(), exists(r.Worktree))
	}
}

// Nothing picked, no estimate, no model in the settings: Mako's agent starts with Sonnet, the model the board showed,
// and the task keeps it as the model that did the work.
func TestTerminalDefaultModel(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\nexec sleep 30\n"), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir, cfg.Terminal.View = "terminal", filepath.Join(bin, "claude"), t.TempDir(), "terminal"
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	d.BoardURL = "http://127.0.0.1:1"

	d.Poll(ctx)
	before, _ := d.Store.Get("grp/app#5")
	if m := StartModel(d.Config(), before); m != "sonnet" {
		t.Fatalf("start model %q", m)
	}
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	defer d.StopTerminal(r.ID)
	if r.Status != core.HandedOff || r.AgentModel != "sonnet" {
		t.Fatalf("status %s, agent model %q, error %q", r.Status, r.AgentModel, r.Error)
	}
	var args string
	for i := 0; i < 200 && !strings.HasSuffix(args, "sonnet\n"); i++ {
		b, _ := os.ReadFile(argsFile)
		args = string(b)
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasSuffix(args, "--model\nsonnet\n") {
		t.Fatalf("args %q", args)
	}
}

func TestChatHandOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // where Claude Code keeps the transcripts a resume reads
	d, _, _ := setup(t)
	ctx := context.Background()
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	// A stand-in for claude -p: it takes the prompt, asks to write a file, ends its turn, then waits.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
echo "$@" > args.txt
read init
read msg
echo "$msg" > prompt.json
echo '{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5-5"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"npm test"}}}'
read answer
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Готово, тести проходять"}]},"parent_tool_use_id":null}'
echo '{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.01}'
read more
echo "$more" > more.json
`), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	d.Cfg = &cfg
	d.BoardURL = "http://127.0.0.1:1"
	pushed := pushes(t)

	d.Poll(ctx)
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Model = "opus" })
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.HandedOff || r.View != "chat" {
		t.Fatalf("status %s view %q error %q", r.Status, r.View, r.Error)
	}
	defer d.StopTerminal(r.ID)
	s := d.Chats.Get(r.ID)
	waitFor(t, func() bool { st, _, _ := s.State(); return st == "waiting" }, "the chat is not waiting")
	d.watchTerminals(time.Now())
	if a := d.Agents()[r.ID]; a.State != "waiting" || a.Reason != "дозвіл на Bash: npm test" || !a.Live {
		t.Fatalf("agent %+v", a)
	}
	// The chat names the model exactly: it takes the place of the alias Mako asked for.
	if r, _ := d.Store.Get(r.ID); r.AgentModel != "claude-opus-5-5" {
		t.Fatalf("agent model %q", r.AgentModel)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "prompt.json")); !strings.Contains(string(b), "Кнопка оплати не працює") {
		t.Fatalf("prompt %s", b)
	}
	// The notification answers the permission itself: «Дозволити» / «Ні».
	asked := ""
	for _, p := range pushed() {
		if strings.Contains(p, "агент чекає на вас") {
			asked = p
		}
	}
	if !strings.Contains(asked, "Дозволити → /api/permit?id=grp%2Fapp%235&req=r1&decision=allow") || !strings.Contains(asked, "Ні → /api/permit?id=grp%2Fapp%235&req=r1&decision=deny") {
		t.Fatalf("waiting push %q", asked)
	}
	s.Permit("r1", "allow")
	waitFor(t, func() bool { st, _, _ := s.State(); return st == "done" }, "the chat is not done")
	d.watchTerminals(time.Now())
	// Answered elsewhere (here, as on the board): the notification goes away.
	if p := strings.Join(pushed(), "\n"); !strings.Contains(p, "withdraw wait:grp/app#5") {
		t.Fatalf("not withdrawn: %s", p)
	}
	if a := d.Agents()[r.ID]; a.State != "done" || a.Message != "Готово, тести проходять" {
		t.Fatalf("agent after the turn %+v", a)
	}
	// The MR card's requests reach the agent as a message.
	if err := d.sayTo(r.ID, "Полагодь CI"); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get(r.ID); r.AgentModel != "claude-opus-5-5" { // going on with it keeps the exact name
		t.Fatalf("agent model after a message %q", r.AgentModel)
	}
	s.Wait() // its exit is reported before the resume below starts another
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "more.json")); !strings.Contains(string(b), "Полагодь CI") {
		t.Fatalf("more %s", b)
	}
	// The agent is gone (closed while idle, or Mako restarted): your message resumes its own session.
	if r, _ = d.Store.Get(r.ID); r.SessionID != "sess-1" {
		t.Fatalf("session %q", r.SessionID)
	}
	projects := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(r.Worktree, "-"))
	os.MkdirAll(projects, 0o755)
	os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово, тести проходять"}]}}`+"\n"), 0o644)
	os.Remove(filepath.Join(r.Worktree, "prompt.json"))
	if err := d.Say(r.ID, "Ще раз тести"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(filepath.Join(r.Worktree, "prompt.json")); return len(b) > 0 }, "the agent was not resumed")
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "prompt.json")); !strings.Contains(string(b), "Ще раз тести") {
		t.Fatalf("resumed with %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "args.txt")); !strings.Contains(string(b), "--resume sess-1") {
		t.Fatalf("args %s", b)
	}
	// The resumed agent saves the task when it exits: let it finish before the temp dirs go.
	d.StopTerminal(r.ID)
	d.Chats.Get(r.ID).Wait()
}

// A message sent after the idle timer closed the agent, while its process still lingers, is delivered: the
// conversation is resumed with it.
func TestSayAfterIdleClose(t *testing.T) {
	old := chat.IdleAfter
	chat.IdleAfter = 100 * time.Millisecond
	defer func() { chat.IdleAfter = old }()
	home := t.TempDir()
	t.Setenv("HOME", home)
	d, _, _ := setup(t)
	ctx := context.Background()
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	bin := t.TempDir()
	// It ends its turn, then lingers a while after its stdin is closed.
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
echo "$@" > args.txt
read init
read msg
echo "$msg" > prompt.json
echo '{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5-5"}'
echo '{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.01}'
cat > /dev/null
touch stdin-closed
sleep 2
`), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	d.Cfg = &cfg
	d.BoardURL = "http://127.0.0.1:1"
	pushes(t)
	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.HandedOff || r.View != "chat" {
		t.Fatalf("status %s view %q error %q", r.Status, r.View, r.Error)
	}
	s := d.Chats.Get(r.ID)
	defer func() { d.StopTerminal(r.ID); d.Chats.Get(r.ID).Wait() }()
	waitFor(t, func() bool { st, _, _ := s.State(); return st == "done" }, "the chat is not done")
	// The idle timer closes its stdin; the process lingers after that.
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(r.Worktree, "stdin-closed")); return err == nil }, "the idle timer did not close the agent")
	projects := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(r.Worktree, "-"))
	os.MkdirAll(projects, 0o755)
	os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}`+"\n"), 0o644)
	os.Remove(filepath.Join(r.Worktree, "prompt.json"))
	if err := d.Say(r.ID, "Після простою"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(filepath.Join(r.Worktree, "prompt.json")); return len(b) > 0 }, "the message was lost")
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "prompt.json")); !strings.Contains(string(b), "Після простою") {
		t.Fatalf("resumed with %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Worktree, "args.txt")); !strings.Contains(string(b), "--resume sess-1") {
		t.Fatalf("args %s", b)
	}
}
