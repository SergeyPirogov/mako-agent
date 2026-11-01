package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func waitFor(t *testing.T, ok func() bool, why string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
	}
}

// settle waits for the task's reviewer and checks to end, the ones the agent's turn started in the background too.
func settle(t *testing.T, d *Dispatcher, id string) {
	t.Helper()
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.reviewing[id] && !d.checking[id]
	}, "the reviewer or the checks did not end")
}

// checkNow runs the checks before the MR, after and with the ones the agent's turn may have started.
func checkNow(t *testing.T, d *Dispatcher, id string) {
	t.Helper()
	settle(t, d, id)
	d.maybeCheck(id)
	settle(t, d, id)
}

// setConfig changes the settings as Mako's reload does, in a copy: the agent's goroutines read the old ones.
func setConfig(d *Dispatcher, change func(*config.Config)) {
	c := *d.Config()
	change(&c)
	d.cfgMu.Lock()
	d.Cfg = &c
	d.cfgMu.Unlock()
}

func TestAskCodeReadOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := setup(t)
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	os.MkdirAll(filepath.Join(repo, "pay"), 0o755)
	os.WriteFile(filepath.Join(repo, "pay", "pay.go"), []byte("package pay\n\nfunc Charge() {}\n"), 0o644)
	// A stand-in for claude -p: it asks for Bash and for Read, answers, then waits for one more question.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
printf '%s\n' "$@" > "$MAKO_TEST_OUT/args.txt"
read init
read msg
echo "$msg" > "$MAKO_TEST_OUT/question.json"
echo '{"type":"system","subtype":"init","session_id":"sess-q","model":"claude-haiku-5-5"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf pay"}}}'
read bash
echo "$bash" > "$MAKO_TEST_OUT/bash.json"
echo '{"type":"control_request","request_id":"r2","request":{"subtype":"can_use_tool","tool_name":"Read","input":{"file_path":"pay/pay.go"}}}'
read rd
echo "$rd" > "$MAKO_TEST_OUT/read.json"
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Оплата: pay/pay.go:3"}]},"parent_tool_use_id":null}'
echo '{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.001}'
read more
echo "$more" > "$MAKO_TEST_OUT/more.json"
`), 0o755)
	out := t.TempDir()
	t.Setenv("MAKO_TEST_OUT", out)
	cfg := *d.Config()
	cfg.Terminal.Command, cfg.StateDir = filepath.Join(bin, "claude"), t.TempDir()
	d.Cfg = &cfg

	ps := d.CodeProjects()
	if len(ps) != 1 || ps[0].Project != "grp/app" || ps[0].Path != repo {
		t.Fatalf("projects %+v", ps)
	}
	if _, err := d.AskCode("nope", "", "де оплата?"); err == nil {
		t.Fatal("asked a project without a clone")
	}
	id, err := d.AskCode(ps[0].Key, "", "Де в нас обробка оплат?")
	if err != nil || !IsQuestion(id) {
		t.Fatalf("ask %q %v", id, err)
	}
	s := d.Chats.Get(id)
	defer s.Stop()
	waitFor(t, func() bool { st, _, _ := s.State(); return st == "done" }, "no answer")

	// Read-only: the reading tools only, nothing that writes or runs, no MCP servers; no worktree, the clone.
	b, _ := os.ReadFile(filepath.Join(out, "args.txt"))
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	tools := args[slices.Index(args, "--tools")+1]
	if tools != "Read,Grep,Glob" || !slices.Contains(args, "--strict-mcp-config") || !slices.Contains(args, "haiku") {
		t.Fatalf("args %q", args)
	}
	for _, w := range []string{"Edit", "Write", "Bash"} {
		if strings.Contains(tools, w) || !slices.Contains(args, w) { // not available, and disallowed
			t.Fatalf("%s in args %q", w, args)
		}
	}
	if s.Worktree != repo {
		t.Fatalf("runs in %q", s.Worktree)
	}
	// Anything but reading is refused at once, nobody is asked.
	if b, _ := os.ReadFile(filepath.Join(out, "bash.json")); !strings.Contains(string(b), `"behavior":"deny"`) {
		t.Fatalf("bash answered %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "read.json")); !strings.Contains(string(b), `"behavior":"allow"`) {
		t.Fatalf("read answered %s", b)
	}
	if s.Asked() != nil {
		t.Fatal("a question waits for the developer")
	}
	if b, _ := os.ReadFile(filepath.Join(out, "question.json")); !strings.Contains(string(b), "обробка оплат") {
		t.Fatalf("question %s", b)
	}
	if !exists(filepath.Join(repo, "pay", "pay.go")) {
		t.Fatal("the clone changed")
	}

	// The project keeps its question, with the conversation to go on with.
	th := d.CodeThreads(ps[0].Key)
	if len(th) != 1 || th[0].ID != id || th[0].Title != "Де в нас обробка оплат?" || th[0].Session != "sess-q" {
		t.Fatalf("threads %+v", th)
	}
	if again, err := d.AskCode(ps[0].Key, id, "А повернення?"); err != nil || again != id {
		t.Fatalf("again %q %v", again, err)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(out, "more.json"))
		return strings.Contains(string(b), "А повернення?")
	}, "no second question")

	// The board opens the files the answer points to, and nothing outside the clone.
	git(t, repo, "add", "pay")
	for _, p := range []string{"pay/pay.go", "./pay/pay.go", "pay.go"} { // an answer may name it short
		if text, found, err := d.CodeFile(context.Background(), ps[0].Key, p); err != nil || found != "pay/pay.go" || !strings.Contains(text, "func Charge") {
			t.Fatalf("%s: %q %q %v", p, found, text, err)
		}
	}
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("x"), 0o644)
	os.Symlink(secret, filepath.Join(repo, "link"))
	for _, p := range []string{"../app/../../secret", secret, "link", "pay"} {
		if _, _, err := d.CodeFile(context.Background(), ps[0].Key, p); err == nil {
			t.Fatalf("%s opened", p)
		}
	}

	// After Mako restarted (no agent): the question shows from its own transcript, never another one of the clone.
	s.Stop()
	s.Wait()
	d.Chats = chat.NewManager("")
	projects := filepath.Join(os.Getenv("HOME"), ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(repo, "-"))
	os.MkdirAll(projects, 0o755)
	os.WriteFile(filepath.Join(projects, "yours.jsonl"), []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"чужа розмова"}]}}`+"\n"), 0o644)
	if _, ok := d.QuestionChat(id); ok {
		t.Fatal("showed another conversation of the clone")
	}
	os.WriteFile(filepath.Join(projects, "sess-q.jsonl"), []byte(`{"type":"user","message":{"content":"Де в нас обробка оплат?"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Оплата: pay/pay.go:3"}]}}
`), 0o644)
	snap, ok := d.QuestionChat(id)
	if !ok || snap.Live || len(snap.Items) != 2 || snap.Items[1].Text != "Оплата: pay/pay.go:3" {
		t.Fatalf("replayed %v %+v", ok, snap)
	}
	if _, ok := d.QuestionChat("grp/app#5"); ok {
		t.Fatal("a task is not a question")
	}
}
