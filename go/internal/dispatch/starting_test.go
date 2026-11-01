package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// While Mako prepares the start the task is “starting” with its steps, not “in the queue”; the journal has «взято в
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
	all := logged(d, id)
	lines := strings.Join(all, "\n")
	taken, tracker := slices.Index(all, "taken into work"), slices.IndexFunc(all, func(l string) bool { return strings.HasPrefix(l, "updating the task in the tracker · ") })
	if taken < 0 || tracker < taken {
		t.Fatalf("«taken into work» not before the start's steps:\n%s", lines)
	}
	if strings.Contains(lines, "in the queue") ||
		!strings.Contains(lines, "updating the task in the tracker · ") || !strings.Contains(lines, "Orca is creating the worktree and starting the agent · ") {
		t.Fatalf("journal:\n%s", lines)
	}
}

// “in the queue” only when something holds the task: the WIP limit here, said in the journal and by QueueReason.
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
	if why := d.QueueReason(); why != "WIP limit: 2 of 2 in work" {
		t.Fatalf("reason %q", why)
	}
	if lines := strings.Join(logged(d, "grp/app#12"), "\n"); !strings.Contains(lines, "in the queue: WIP limit: 2 of 2 in work") {
		t.Fatalf("journal:\n%s", lines)
	}
	if lines := strings.Join(logged(d, "grp/app#10"), "\n"); strings.Contains(lines, "in the queue") {
		t.Fatalf("started at once, yet “in the queue”:\n%s", lines)
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
	if lines := strings.Join(logged(d, id), "\n"); !strings.Contains(lines, "start cancelled, worktree removed") {
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
	// The branch this start made goes with it.
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", r.BranchName).Output(); r.BranchName == "" || len(out) != 0 {
		t.Fatalf("branch %q left: %s", r.BranchName, out)
	}
	if lines := strings.Join(logged(d, id), "\n"); !strings.Contains(lines, "updating main · ") || !strings.Contains(lines, "creating the worktree · ") ||
		!strings.Contains(lines, "start cancelled, worktree removed") {
		t.Fatalf("journal:\n%s", lines)
	}
}

// terminalStart: grp/app#5 for Mako's own agent in the terminal, its repo with a commit; claude only waits.
func terminalStart(t *testing.T, setupCmd string) (d *Dispatcher, repo string) {
	d, _, _ = setup(t)
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir, cfg.Terminal.View = "terminal", filepath.Join(bin, "claude"), t.TempDir(), "terminal"
	cfg.Terminal.SetupCommand = setupCmd
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	d.BoardURL = "http://127.0.0.1:1"
	d.Poll(context.Background())
	return d, repo
}

// «Скасувати» while the base branch is fetched: no worktree, no branch, the task cancelled.
func TestCancelWhileFetching(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), "ssh")
	os.WriteFile(ssh, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755) // origin never answers
	t.Setenv("GIT_SSH_COMMAND", ssh)
	d, repo := terminalStart(t, "-")
	id := "grp/app#5"
	d.Take(context.Background(), id)
	waitFor(t, func() bool {
		p := d.Starting(id)
		return p != nil && len(p.Steps) > 0 && p.Steps[len(p.Steps)-1].Key == "main"
	}, "the fetch did not begin")
	if p := d.Starting(id); p.Base != "main" {
		t.Fatalf("plan %+v", p)
	}
	began := time.Now()
	d.Cancel(id)
	d.Wait()
	if time.Since(began) > 10*time.Second {
		t.Fatal("the fetch was not stopped")
	}
	r, _ := d.Store.Get(id)
	wts, _ := filepath.Glob(filepath.Join(filepath.Dir(repo), ".worktrees", "app", "*"))
	if r.Status != core.Cancelled || len(wts) != 0 || d.Sessions.Get(id) != nil {
		t.Fatalf("status %s, worktrees %v", r.Status, wts)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", r.BranchName).Output(); len(out) != 0 {
		t.Fatalf("branch left: %s", out)
	}
	if lines := strings.Join(logged(d, id), "\n"); !strings.Contains(lines, "start cancelled") || !strings.Contains(lines, "cancelled during start") {
		t.Fatalf("journal:\n%s", lines)
	}
}

// A setup that leaves something running in the background does not hold the start; its output reaches the
// terminal before the agent.
func TestSetupInBackgroundDoesNotHang(t *testing.T) {
	d, _ := terminalStart(t, "echo prepared; sleep 30 &")
	id := "grp/app#5"
	done := make(chan struct{})
	go func() { d.Take(context.Background(), id); d.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the start waits for the setup's background process")
	}
	r, _ := d.Store.Get(id)
	if r.Status != core.HandedOff {
		t.Fatalf("status %s, error %q", r.Status, r.Error)
	}
	// The agent's exit saves the task: let it finish before the temp dirs go.
	defer func() { s := d.Sessions.Get(id); d.StopTerminal(id); s.Wait() }()
	waitFor(t, func() bool { return strings.Contains(d.Sessions.Get(id).Output(4000), "prepared") }, "the setup's output is not in the terminal")
}
