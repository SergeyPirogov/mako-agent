package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// blockHand: Orca's hand-off waits until release is closed; started is closed once it is called.
func blockHand(d *Dispatcher, worktree string) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	d.Hand = func(ctx context.Context, o config.Orca, j orca.Job, log func(string)) (*orca.Result, error) {
		once.Do(func() { close(started) })
		<-release
		return &orca.Result{Worktree: worktree, Branch: j.Branch}, nil
	}
	return started, release
}

func logged(d *Dispatcher, id string) []string {
	var out []string
	for _, l := range d.Logs(id) {
		out = append(out, l.Text)
	}
	return out
}

// While Mako prepares the start the task is «запускаю» with its steps, not «у черзі»; the journal has «взято в
// роботу» and the steps' times.
func TestStartingSteps(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	id := "grp/app#5"
	d.Poll(ctx)
	started, release := blockHand(d, filepath.Join(t.TempDir(), "wt"))
	d.Take(ctx, id)
	<-started
	p := d.Starting(id)
	if r, _ := d.Store.Get(id); r.Status != core.Queued || p == nil {
		t.Fatalf("status %s, starting %+v", r.Status, p)
	}
	if p.Executor != "orca" || len(p.Steps) != 2 || p.Steps[0].Key != "tracker" || p.Steps[0].Done == 0 || p.Steps[1].Key != "orca" || p.Steps[1].Done != 0 {
		t.Fatalf("steps %+v", p)
	}
	if why := d.QueueReason(); why != "" {
		t.Fatalf("nothing holds the queue, yet: %q", why)
	}
	close(release)
	d.Wait()
	if r, _ := d.Store.Get(id); r.Status != core.HandedOff || d.Starting(id) != nil {
		t.Fatalf("after the start: %s, %+v", r.Status, d.Starting(id))
	}
	lines := strings.Join(logged(d, id), "\n")
	if !strings.HasPrefix(logged(d, id)[1], "взято в роботу") || strings.Contains(lines, "у черзі") ||
		!strings.Contains(lines, "оновлюю задачу в трекері · ") || !strings.Contains(lines, "Orca створює worktree і запускає агента · ") {
		t.Fatalf("journal:\n%s", lines)
	}
}

// «у черзі» only when something holds the task: the WIP limit here, said in the journal and by QueueReason.
func TestQueuedOnlyOverWip(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	for i := 10; i < 13; i++ {
		f.issues[i] = map[string]any{"id": 1000 + i, "iid": i, "state": "opened", "title": fmt.Sprintf("Task %d", i), "web_url": "u",
			"labels": []string{}, "assignees": []any{}, "references": map[string]string{"full": fmt.Sprintf("grp/app#%d", i)}}
	}
	d.Poll(ctx)
	for i := 10; i < 13; i++ {
		d.Take(ctx, fmt.Sprintf("grp/app#%d", i))
		d.Wait()
	}
	if d.Starting("grp/app#12") != nil {
		t.Fatal("a task over the WIP limit is shown as starting")
	}
	if why := d.QueueReason(); why != "WIP-ліміт: 2 з 2 в роботі" {
		t.Fatalf("reason %q", why)
	}
	if lines := strings.Join(logged(d, "grp/app#12"), "\n"); !strings.Contains(lines, "у черзі: WIP-ліміт: 2 з 2 в роботі") {
		t.Fatalf("journal:\n%s", lines)
	}
	if lines := strings.Join(logged(d, "grp/app#10"), "\n"); strings.Contains(lines, "у черзі") {
		t.Fatalf("started at once, yet «у черзі»:\n%s", lines)
	}
}

// «Скасувати» while Orca starts the task: the task stays cancelled and Orca's workspace goes.
func TestCancelWhileStartingOrca(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	id := "grp/app#5"
	d.Poll(ctx)
	wt := filepath.Join(t.TempDir(), "wt")
	started, release := blockHand(d, wt)
	var removed []string
	d.RemoveWorkspace = func(ctx context.Context, repoPath, worktree, branch string) error {
		removed = append(removed, worktree)
		return nil
	}
	d.Take(ctx, id)
	<-started
	d.Cancel(id)
	close(release)
	d.Wait()
	r, _ := d.Store.Get(id)
	if r.Status != core.Cancelled || r.Worktree != "" || len(removed) != 1 || removed[0] != wt {
		t.Fatalf("status %s, worktree %q, removed %v", r.Status, r.Worktree, removed)
	}
	if lines := strings.Join(logged(d, id), "\n"); !strings.Contains(lines, "запуск скасовано, worktree прибрано") {
		t.Fatalf("journal:\n%s", lines)
	}
}

// «Скасувати» during the setup of Mako's own agent: the setup is stopped, no agent starts, the worktree goes.
func TestCancelWhileSetup(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	id := "grp/app#5"
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir, cfg.Terminal.View = "terminal", filepath.Join(bin, "claude"), t.TempDir(), "terminal"
	cfg.Terminal.SetupCommand = "touch setup-began; sleep 30"
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	d.BoardURL = "http://127.0.0.1:1"
	d.Poll(ctx)

	d.Take(ctx, id)
	var wt string
	waitFor(t, func() bool {
		p := d.Starting(id)
		if p == nil || len(p.Steps) == 0 || p.Steps[len(p.Steps)-1].Key != "setup" {
			return false
		}
		m, _ := filepath.Glob(filepath.Join(filepath.Dir(repo), ".worktrees", "app", "*", "setup-began"))
		if len(m) == 1 {
			wt = filepath.Dir(m[0])
		}
		return wt != ""
	}, "the setup did not begin")
	if p := d.Starting(id); !p.Setup || p.Executor != terminalExecutor {
		t.Fatalf("plan %+v", p)
	}
	began := time.Now()
	d.Cancel(id)
	d.Wait()
	if time.Since(began) > 10*time.Second {
		t.Fatal("the setup was not stopped")
	}
	r, _ := d.Store.Get(id)
	if r.Status != core.Cancelled || exists(wt) || d.Sessions.Get(id) != nil {
		t.Fatalf("status %s, worktree there %v, agent %v", r.Status, exists(wt), d.Sessions.Get(id) != nil)
	}
	if lines := strings.Join(logged(d, id), "\n"); !strings.Contains(lines, "оновлюю main · ") || !strings.Contains(lines, "створюю worktree · ") ||
		!strings.Contains(lines, "запуск скасовано, worktree прибрано") {
		t.Fatalf("journal:\n%s", lines)
	}
}
