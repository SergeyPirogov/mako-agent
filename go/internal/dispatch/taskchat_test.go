package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// A task whose agent is not running shows its conversation from the transcript, with the steps relative to its
// worktree; a step that finished later is seen though no item was added.
func TestTaskChat(t *testing.T) {
	d, _, _ := setup(t)
	conf, wt := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(dir, 0o700)
	file := filepath.Join(dir, "s-1.jsonl")
	os.WriteFile(file, []byte(`{"type":"user","message":{"content":"Run the tests"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cd `+wt+` && go test ./..."}}]}}
`), 0o600)

	if _, ok := d.TaskChat("t#1"); ok {
		t.Fatal("a task not in a terminal has no replayed chat")
	}
	d.Store.Update("t#1", func(r *core.Record) { r.Worktree, r.Executor = wt, terminalExecutor })
	first, ok := d.TaskChat("t#1")
	if !ok || len(first.Turns) != 2 || first.Turns[1].Blocks[0].Rows[0].Label != "go test ./..." {
		t.Fatalf("replayed %+v", first.Turns)
	}

	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok  \tx\t0.1s"}]}}` + "\n")
	f.Close()
	again, _ := d.TaskChat("t#1")
	if again.Version == first.Version || len(again.Items) != len(first.Items) || again.Items[1].Status != "ok" {
		t.Fatalf("a finished step not seen: version %d → %d, %+v", first.Version, again.Version, again.Items)
	}

	// Tasks gone are not kept.
	d.Store.Update("t#1", func(r *core.Record) { r.Executor = "" })
	d.replayMu.Lock()
	d.pruneReplays()
	n := len(d.replays)
	d.replayMu.Unlock()
	if n != 0 {
		t.Fatalf("kept %d replays of tasks gone", n)
	}
}

// A question's worktree being prepared: its message and the setup show as turns.
func TestSetupChatHasTurns(t *testing.T) {
	askSetups.Store("q-setup", pendingSetup{command: "pnpm install", prompt: "Where are payments?"})
	defer askSetups.Delete("q-setup")
	s, ok := setupChat("q-setup")
	if !ok || len(s.Turns) != 2 || s.Turns[0].Who != "user" || s.Turns[1].Blocks[0].Kind != "current" {
		t.Fatalf("turns %+v", s.Turns)
	}
}
