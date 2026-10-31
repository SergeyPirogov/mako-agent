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

// fakeTerminals: one agent terminal per worktree, its screen and what it waits for set by the test.
type fakeTerminals struct {
	mu      sync.Mutex
	last    int64
	wait    *orca.Wait
	screen  []string
	gone    bool
	focused string
}

func (f *fakeTerminals) List(ctx context.Context, o config.Orca, wt string) ([]orca.Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return nil, nil
	}
	return []orca.Terminal{{Handle: "shell", LastOutputAt: f.last}, {Handle: "t1", Agent: "claude", LastOutputAt: f.last}}, nil
}
func (f *fakeTerminals) Waiting(ctx context.Context, o config.Orca, wt, h string) (*orca.Wait, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wait, nil
}
func (f *fakeTerminals) Screen(ctx context.Context, o config.Orca, wt, h string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(append([]string{}, f.screen...), "", ""), nil
}
func (f *fakeTerminals) Tail(ctx context.Context, o config.Orca, wt, h string, n int) ([]string, error) {
	return []string{"Done: fixed the click handler, added a test. All tests pass."}, nil
}
func (f *fakeTerminals) Focus(ctx context.Context, o config.Orca, wt, h string) error {
	f.focused = h
	return nil
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
	term := &fakeTerminals{screen: []string{"● Editing checkout.ts", "  Running tests…"}}
	d.Term = term
	now := time.Now()
	term.last = now.UnixMilli()
	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()

	d.WatchAgents(ctx, now)
	a := d.Agents()["grp/app#5"]
	if a.State != "working" || a.Name != "claude" || len(a.Screen) != 2 {
		t.Fatalf("working: %+v", a)
	}

	term.wait = &orca.Wait{Source: "hook", Reason: "permission", Since: now.UnixMilli()}
	term.screen = []string{"Do you want to run npm test?", "❯ 1. Yes"}
	d.WatchAgents(ctx, now.Add(10*time.Second))
	if a = d.Agents()["grp/app#5"]; a.State != "waiting" || a.Reason != "просить дозволу" {
		t.Fatalf("waiting: %+v", a)
	}
	if logs := d.Logs("grp/app#5"); !strings.Contains(logs[len(logs)-1].Text, "чекає на вас") {
		t.Fatalf("not told: %+v", logs)
	}

	term.wait = nil
	d.WatchAgents(ctx, now.Add(5*time.Minute))
	if a = d.Agents()["grp/app#5"]; a.State != "quiet" {
		t.Fatalf("quiet: %+v", a)
	}

	if err := d.FocusAgent(ctx, "grp/app#5"); err != nil || term.focused != "t1" {
		t.Fatalf("focus %v %q", err, term.focused)
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
	d.Term = &fakeTerminals{}
	d.Cfg.Orca.SummaryComment = true
	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
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
	if !strings.Contains(f.notes[len(f.notes)-1], "що зробив агент") {
		t.Fatalf("no summary comment: %v", f.notes)
	}
	_ = core.Done
}
