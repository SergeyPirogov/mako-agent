package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// A chat without a task: its own worktree on a new branch from main, the agent with the terminal's settings and
// the first message; a message after the agent stopped resumes its session; «×» takes the worktree away, the branch
// stays.
func TestFreeChat(t *testing.T) {
	d, repo, conf, out := freeChatSetup(t)
	p := d.CodeProjects()[0]
	if _, err := d.NewChat(p.Key, "  "); err == nil {
		t.Fatal("a chat without a message")
	}
	if _, err := d.NewChat("nope", "Привіт"); err == nil {
		t.Fatal("a chat in a project without a clone")
	}
	id, err := d.NewChat(p.Key, "Спробуй швидший кеш")
	if err != nil || !IsFreeChat(id) {
		t.Fatalf("new %q %v", id, err)
	}
	waitFor(t, func() bool {
		cs := d.FreeChats()
		return len(cs) == 1 && cs[0].Session == "sess-c"
	}, "the chat did not keep its session")
	c := d.FreeChats()[0]
	if c.ID != id || c.Branch != "mako/"+id || c.Title != "Спробуй швидший кеш" || !exists(filepath.Join(c.Dir, "a.txt")) {
		t.Fatalf("chat %+v", c)
	}
	if b := gitOut(t, c.Dir, "branch", "--show-current"); b != c.Branch {
		t.Fatalf("worktree on %q", b)
	}
	real, _ := filepath.EvalSymlinks(c.Dir)
	if b, _ := os.ReadFile(filepath.Join(out, "pwd.txt")); strings.TrimSpace(string(b)) != real {
		t.Fatalf("agent ran in %q, not %q", b, real)
	}
	// Its agent knows its chat, as a task's agent knows its task.
	if b, _ := os.ReadFile(filepath.Join(out, "task.txt")); strings.TrimSpace(string(b)) != id {
		t.Fatalf("MAKO_TASK %q", b)
	}
	runs := func() [][]string {
		b, _ := os.ReadFile(filepath.Join(out, "args.txt"))
		var all [][]string
		for _, r := range strings.Split(strings.TrimSuffix(string(b), "---\n"), "---\n") {
			all = append(all, strings.Split(strings.TrimSpace(r), "\n"))
		}
		return all
	}
	first := runs()[0]
	// The routes' work of a task with no estimate.
	route := routeFor(d.Config(), core.Record{}, core.PhaseWork, "")
	if first[slices.Index(first, "--model")+1] != route.Model || route.Effort != "" && first[slices.Index(first, "--effort")+1] != route.Effort || !slices.Contains(first, "Bash(go test:*)") || slices.Contains(first, "--resume") ||
		slices.Contains(first, "--disallowedTools") {
		t.Fatalf("args %q", first)
	}

	// Mako restarted: the conversation shows from its transcript, and a message resumes it.
	stopChat(d.Chats.Get(id))
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(c.Dir, "-"))
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "sess-c.jsonl"), []byte(`{"type":"user","message":{"content":"Спробуй швидший кеш"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}
`), 0o600)
	if s, ok := d.FreeChatSnapshot(id); !ok || len(s.Turns) != 2 {
		t.Fatalf("replayed %v %+v", ok, s.Turns)
	}
	if err := d.SayFree(id, "А тепер тести"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(runs()) == 2 }, "the message did not resume the chat")
	if again := runs()[1]; again[slices.Index(again, "--resume")+1] != "sess-c" {
		t.Fatalf("args %q", again)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(out, "msgs.txt"))
		return strings.Contains(string(b), "А тепер тести")
	}, "the message did not reach the agent")

	// «×»: the agent stops, the worktree goes, the branch stays.
	if err := d.RemoveFreeChat(id); err != nil {
		t.Fatal(err)
	}
	// The board no longer gets its conversation.
	if exists(c.Dir) || len(d.FreeChats()) != 0 || d.Chats.Get(id) != nil {
		t.Fatalf("not removed: %v %+v", exists(c.Dir), d.FreeChats())
	}
	if !gitx.HasBranch(context.Background(), repo, c.Branch) {
		t.Fatal("the branch was deleted")
	}
	if err := d.SayFree(id, "ще"); err == nil {
		t.Fatal("wrote to a removed chat")
	}
}

// freeChatSetup: a project with its clone and remote on main, and a stand-in for claude in the terminal settings.
func freeChatSetup(t *testing.T) (d *Dispatcher, repo, conf, out string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	conf = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	d, _, _ = setup(t)
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	remote := filepath.Join(t.TempDir(), "app.git")
	git(t, repo, "init", "-q", "--bare", "-b", "main", remote)
	git(t, repo, "remote", "set-url", "origin", remote)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644)
	git(t, repo, "add", "a.txt")
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "a")
	git(t, repo, "push", "-q", "origin", "main")

	// A stand-in for claude -p: each run notes its arguments, folder and message, answers, and waits for more.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
{ printf '%s\n' "$@"; echo ---; } >> "$MAKO_TEST_OUT/args.txt"
pwd -P > "$MAKO_TEST_OUT/pwd.txt"
echo "$MAKO_TASK" > "$MAKO_TEST_OUT/task.txt"
read init
read msg
echo "$msg" >> "$MAKO_TEST_OUT/msgs.txt"
echo '{"type":"system","subtype":"init","session_id":"sess-c","model":"claude-sonnet-5-5"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]},"parent_tool_use_id":null}'
echo '{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.001}'
read more
`), 0o755)
	out = t.TempDir()
	t.Setenv("MAKO_TEST_OUT", out)
	setConfig(d, func(c *config.Config) {
		c.Terminal.Command, c.StateDir = filepath.Join(bin, "claude"), t.TempDir()
		c.Terminal.AllowedTools = []string{"Bash(go test:*)"}
	})

	return d, repo, conf, out
}

// A chat removed while its worktree is prepared: its agent does not start, its worktree goes.
func TestFreeChatRemovedWhileSetup(t *testing.T) {
	d, _, _, out := freeChatSetup(t)
	gate := filepath.Join(t.TempDir(), "go")
	setConfig(d, func(c *config.Config) {
		c.Terminal.SetupCommand = `while [ ! -e "` + gate + `" ]; do sleep 0.05; done`
	})
	id, err := d.NewChat(d.CodeProjects()[0].Key, "Привіт")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := d.FreeChatSnapshot(id); !ok || len(s.Items) != 2 || s.Items[1].Tool != "Setup" {
		t.Fatalf("no step while it is made: %+v", s.Items)
	}
	waitFor(t, func() bool { c, _ := d.freeChat(id); return c.Dir != "" }, "the worktree was not made")
	c, _ := d.freeChat(id)
	if err := d.RemoveFreeChat(id); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(gate, nil, 0o644)
	waitFor(t, func() bool { _, pending := askSetups.Load(id); return !pending }, "the setup did not end")
	if d.Chats.Get(id) != nil || exists(c.Dir) || exists(filepath.Join(out, "args.txt")) {
		t.Fatalf("started in a removed chat: %v %v", d.Chats.Get(id) != nil, exists(c.Dir))
	}
	if _, _, err := d.Attach(id, "a.png", []byte("x")); err == nil {
		t.Fatal("attached to a removed chat")
	}
}

// A picture attached in a chat without a task is kept for its agent; a chat Mako does not have takes none.
func TestFreeChatAttach(t *testing.T) {
	d, _, _ := setup(t)
	setConfig(d, func(c *config.Config) { c.StateDir = t.TempDir() })
	d.updateChats(func(all []FreeChat) []FreeChat { return append(all, FreeChat{ID: "chat-1", Error: "no worktree"}) })
	if path, _, err := d.Attach("chat-1", "shot.png", []byte("png")); err != nil || !exists(path) {
		t.Fatalf("attach %q %v", path, err)
	}
	if _, _, err := d.Attach("chat-2", "shot.png", []byte("png")); err == nil {
		t.Fatal("attached to a chat Mako does not have")
	}
	// A chat whose worktree could not be made shows why and does not go on.
	if s, ok := d.FreeChatSnapshot("chat-1"); !ok || len(s.Items) != 2 || s.Items[1].Kind != "error" {
		t.Fatalf("snapshot %+v", s.Items)
	}
	if err := d.SayFree("chat-1", "ще"); err == nil || err.Error() != "no worktree" {
		t.Fatalf("say %v", err)
	}
}

// A plan handed over to a new chat: its first line names the chat, and the agent gets the whole text, with the line
// that says where the plan comes from.
func TestNewChatFromPlan(t *testing.T) {
	d, _, _, out := freeChatSetup(t)
	p := d.CodeProjects()[0]
	text := "Execute this plan: Faster cache\n\n1. Add the cache\n2. Test it\n\n_Plan from task grp/app#3 \"Cache\"._"
	id, err := d.NewChat(p.Key, text)
	if err != nil {
		t.Fatal(err)
	}
	if cs := d.FreeChats(); len(cs) != 1 || cs[0].ID != id || cs[0].Title != "Execute this plan: Faster cache" {
		t.Fatalf("chats %+v", cs)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(out, "msgs.txt"))
		return strings.Contains(string(b), "_Plan from task grp/app#3")
	}, "the plan did not reach the agent")
	// The chat's agent and worktree live on in the test's folders: wait for the agent to be up and take the chat away
	// before they are removed, or their cleanup races the worktree ("directory not empty").
	waitFor(t, func() bool { c, _ := d.freeChat(id); return c.Session == "sess-c" }, "the chat did not keep its session")
	if err := d.RemoveFreeChat(id); err != nil {
		t.Fatal(err)
	}
}
