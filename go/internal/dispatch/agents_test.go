package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// fakeTerminals: Orca with one workspace (the task's, found by branch) whose agent the test moves along.
type fakeTerminals struct {
	mu      sync.Mutex
	ws      *orca.Workspace
	pty     bool
	screen  []string
	focused string
}

func (f *fakeTerminals) Workspaces(ctx context.Context, o config.Orca) ([]orca.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ws == nil {
		return []orca.Workspace{{Path: "/elsewhere", Branch: "other"}}, nil
	}
	return []orca.Workspace{{Path: "/elsewhere", Branch: "other"}, *f.ws}, nil
}
func (f *fakeTerminals) List(ctx context.Context, o config.Orca, wt, branch string) ([]orca.Terminal, int, error) {
	if !f.pty {
		return nil, 0, nil
	}
	return []orca.Terminal{{Handle: "t1", Agent: "codex"}}, 1, nil
}
func (f *fakeTerminals) Screen(ctx context.Context, o config.Orca, wt, h string) ([]string, error) {
	return append(append([]string{}, f.screen...), "", ""), nil
}
func (f *fakeTerminals) Tail(ctx context.Context, o config.Orca, wt, h string, n int) ([]string, error) {
	return []string{"terminal tail"}, nil
}
func (f *fakeTerminals) Focus(ctx context.Context, o config.Orca, wt, h string) error {
	f.focused = h
	return nil
}

func (f *fakeTerminals) agent(status string, row orca.AgentRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws.Status = status
	f.ws.Agents = []orca.AgentRow{{PaneKey: "sub", ParentPaneKey: "main", State: "working"}, row}
}

// fakeClaude answers every prompt with a fixed summary and keeps the prompt for the test.
func fakeClaude(t *testing.T) string {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt")
	script := "#!/bin/sh\ncat > " + prompt + "\necho '## Що зроблено'\necho '- виправлено кнопку'\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	old := writer.Claude
	writer.Claude = filepath.Join(dir, "claude")
	t.Cleanup(func() { writer.Claude = old })
	return prompt
}

func TestWatchAgents(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	term := &fakeTerminals{}
	d.Term = term
	now := time.Now()
	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	// Orca knows the workspace under another spelling of the path: the branch finds it.
	term.ws = &orca.Workspace{Path: "/private" + r.Worktree, Branch: "refs/heads/" + r.Branch}

	// A chat agent (no terminal) at work.
	term.agent("working", orca.AgentRow{AgentType: "claude", State: "working", ToolName: "Read", ToolInput: "summary.md", StateStartedAt: now.UnixMilli() - 60000})
	d.WatchAgents(ctx, now)
	a := d.Agents()["grp/app#5"]
	if a.State != "working" || a.Name != "claude" || a.Tool != "Read: summary.md" || a.Since != now.UnixMilli()-60000 || a.Screen != nil {
		t.Fatalf("working: %+v", a)
	}

	// It asks to run a command.
	term.agent("permission", orca.AgentRow{AgentType: "claude", State: "blocked", ToolName: "Bash", ToolInput: "npm test -- payment"})
	d.WatchAgents(ctx, now.Add(10*time.Second))
	if a = d.Agents()["grp/app#5"]; a.State != "waiting" || a.Reason != "дозвіл на Bash: npm test -- payment" {
		t.Fatalf("waiting: %+v", a)
	}
	if logs := d.Logs("grp/app#5"); !strings.Contains(logs[len(logs)-1].Text, "чекає на вас") {
		t.Fatalf("not told: %+v", logs)
	}

	// Its turn is over: what it said is kept for the card.
	term.agent("done", orca.AgentRow{AgentType: "claude", State: "done", LastAssistantMessage: "## Готово\nMR: !9"})
	d.WatchAgents(ctx, now.Add(20*time.Second))
	if a = d.Agents()["grp/app#5"]; a.State != "done" || a.Message != "## Готово\nMR: !9" || a.Tool != "" {
		t.Fatalf("done: %+v", a)
	}
	if logs := d.Logs("grp/app#5"); logs[len(logs)-1].Text != "агент закінчив" {
		t.Fatalf("not told: %+v", logs)
	}

	// Interrupted.
	term.agent("done", orca.AgentRow{AgentType: "claude", State: "done", Interrupted: true})
	d.WatchAgents(ctx, now.Add(30*time.Second))
	if a = d.Agents()["grp/app#5"]; a.State != "stopped" || a.Reason != "перервано" {
		t.Fatalf("stopped: %+v", a)
	}

	// An agent in a terminal shows its screen too.
	term.pty, term.screen = true, []string{"› Run tests?"}
	term.agent("working", orca.AgentRow{AgentType: "codex", State: "working", HasLivePty: true})
	d.WatchAgents(ctx, now.Add(40*time.Second))
	if a = d.Agents()["grp/app#5"]; len(a.Screen) != 1 {
		t.Fatalf("screen: %+v", a)
	}
	if err := d.FocusAgent(ctx, "grp/app#5"); err != nil || term.focused != "t1" {
		t.Fatalf("focus %v %q", err, term.focused)
	}

	// The workspace is gone from Orca.
	term.ws = nil
	d.WatchAgents(ctx, now)
	if a = d.Agents()["grp/app#5"]; a.State != "missing" {
		t.Fatalf("missing: %+v", a)
	}

	// Out of Orca: no longer watched.
	d.Cancel("grp/app#5")
	d.WatchAgents(ctx, now)
	if _, ok := d.Agents()["grp/app#5"]; ok {
		t.Fatal("still watched after cancel")
	}
}

func TestSummaryWhenMRAppears(t *testing.T) {
	d, f, _ := setup(t)
	prompt := fakeClaude(t)
	ctx := context.Background()
	term := &fakeTerminals{}
	d.Term = term
	d.Cfg.Orca.SummaryComment = true
	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	term.ws = &orca.Workspace{Path: r.Worktree, Branch: r.Branch}
	term.agent("done", orca.AgentRow{State: "done", LastAssistantMessage: "Done: fixed the click handler, added a test. All tests pass."})
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "opened", "web_url": f.base + "/grp/app/-/merge_requests/9", "source_branch": r.Branch,
		"title": "Fix the payment button", "description": strings.Repeat("a real description ", 10)})
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	r, _ = d.Store.Get("grp/app#5")
	if !strings.Contains(r.Summary, "виправлено кнопку") {
		t.Fatalf("summary %q", r.Summary)
	}
	sent, _ := os.ReadFile(prompt)
	if !strings.Contains(string(sent), "All tests pass") {
		t.Fatalf("agent's words not in the prompt:\n%s", sent)
	}
	if !strings.Contains(f.notes[len(f.notes)-1], "Що зроблено:") {
		t.Fatalf("no summary comment: %v", f.notes)
	}
	_ = core.Done
}
