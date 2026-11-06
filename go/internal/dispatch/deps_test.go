package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func loggedTimes(d *Dispatcher, id, s string) int {
	n := 0
	for _, l := range d.Logs(id) {
		if strings.Contains(l.Text, s) {
			n++
		}
	}
	return n
}

// A chain A → B → C taken at once starts in its order: A alone, though the WIP limit has room; B once A has an MR,
// from A's branch; C waits for B. A task waiting takes no place in the WIP limit.
func TestQueueOrderOfAChain(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	cfg := *d.Config()
	cfg.WipLimit = 3
	d.Cfg = &cfg
	a, b, c := "grp/app#5", "grp/app#6", "grp/app#7"
	for _, n := range []int{6, 7} {
		f.issues[n] = map[string]any{"id": 1000 + n, "iid": n, "state": "opened", "title": "Частина", "web_url": fmt.Sprintf("%s/grp/app/-/issues/%d", f.base, n),
			"assignees": []any{}, "references": map[string]string{"full": fmt.Sprintf("grp/app#%d", n)}}
	}
	d.Poll(ctx)
	d.Store.Update(b, func(r *core.Record) { r.After, r.StackOn = []string{a}, a })
	d.Store.Update(c, func(r *core.Record) { r.After, r.StackOn = []string{b}, b })
	for _, id := range []string{c, b, a} {
		d.Take(ctx, id)
	}
	d.Wait()
	if len(*jobs) != 1 || (*jobs)[0].Task.ID != a {
		t.Fatalf("started %d tasks, want only %s: %+v", len(*jobs), a, *jobs)
	}
	if w := d.WaitsFor(mustGet(t, d, b)); w == nil || w.On != a || w.Why != "mr" {
		t.Fatalf("%s waits for %+v", b, w)
	}
	if loggedTimes(d, b, "in the queue: waits for an MR of "+a) != 1 {
		t.Fatalf("the journal does not say what %s waits for: %+v", b, d.Logs(b))
	}

	// A has an MR: B starts from its branch, C still waits for B.
	d.Store.Update(a, func(r *core.Record) {
		r.Status, r.PRURL, r.Branch = core.Done, f.base+"/grp/app/-/merge_requests/1", "fix/5-knopka"
	})
	d.schedule(ctx)
	d.Wait()
	if len(*jobs) != 2 || (*jobs)[1].Task.ID != b || (*jobs)[1].Repo.BaseBranch != "fix/5-knopka" {
		t.Fatalf("B not started from A's branch: %+v", *jobs)
	}
	if rb := mustGet(t, d, b); rb.Stack == nil || rb.Stack.On != a || rb.Stack.Branch != "fix/5-knopka" {
		t.Fatalf("B's stack: %+v", rb.Stack)
	}
	if rc := mustGet(t, d, c); rc.Status != core.Queued {
		t.Fatalf("C started before B has an MR: %s", rc.Status)
	}

	// A's MR closed without merging: C's chain waits, and the board says why.
	d.Store.Update(a, func(r *core.Record) { r.ReviewState = "closed" })
	d.Store.Update(c, func(r *core.Record) { r.After = []string{a, b} })
	if w := d.WaitsFor(mustGet(t, d, c)); w == nil || w.On != a || w.Why != "closed" {
		t.Fatalf("C waits for %+v", w)
	}
}

// A blocker the tracker links («is blocked by») that is not on the board holds the task in the queue, and
// «Запустити зараз» too; once the link is gone (the blocker closed) the task starts by itself.
func TestTrackerBlockerHoldsTheQueue(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	id := "grp/app#5"
	f.links = map[int][]map[string]any{5: {{"iid": 9, "state": "opened", "link_type": "is_blocked_by", "references": map[string]string{"full": "grp/app#9"}}}}
	d.Poll(ctx)
	if r := mustGet(t, d, id); len(r.Task.BlockedBy) != 1 || r.Task.BlockedBy[0] != "grp/app#9" {
		t.Fatalf("blocked by %v", r.Task.BlockedBy)
	}
	if w := d.WaitsFor(mustGet(t, d, id)); w == nil || w.Why != "open" || w.On != "grp/app#9" {
		t.Fatalf("new task waits for %+v", w)
	}
	d.Take(ctx, id)
	d.Wait()
	d.Push(ctx, id)
	d.Wait()
	if len(*jobs) != 0 {
		t.Fatalf("a blocked task started: %+v", *jobs)
	}
	if loggedTimes(d, id, "not started now: blocked by grp/app#9 in the tracker") != 1 {
		t.Fatalf("Push did not say why: %+v", d.Logs(id))
	}
	// The blocker is closed: GitLab lists the link no more.
	f.mu.Lock()
	f.links = nil
	f.mu.Unlock()
	d.Poll(ctx)
	d.schedule(ctx)
	d.Wait()
	if len(*jobs) != 1 {
		t.Fatalf("not started once its blocker closed: %+v, %+v", mustGet(t, d, id).Task.BlockedBy, d.Logs(id))
	}
}

// A blocker on the board holds the task until it is merged, not just until it has an MR (only StackOn starts
// earlier).
func TestBlockerOnTheBoardWaitsForTheMerge(t *testing.T) {
	get := func(recs ...core.Record) func(string) (core.Record, bool) {
		return func(id string) (core.Record, bool) {
			for _, r := range recs {
				if r.ID == id {
					return r, true
				}
			}
			return core.Record{}, false
		}
	}
	blocker := core.Record{ID: "PAY-3", Status: core.Done, PRURL: "u", Branch: "fix/pay-3", Task: core.Task{ID: "PAY-3", Project: "PAY", URL: "https://j/PAY-3"}}
	r := core.Record{ID: "PAY-7", Status: core.Queued, Task: core.Task{ID: "PAY-7", BlockedBy: []string{"PAY-3"}}}
	if w, stack := waitingFor(r, get(blocker)); w == nil || w.Why != "merge" || w.URL != "https://j/PAY-3" || stack != nil {
		t.Fatalf("waits %+v, stack %+v", w, stack)
	}
	blocker.ReviewState = "merged"
	if w, _ := waitingFor(r, get(blocker)); w != nil {
		t.Fatalf("still waits for a merged blocker: %+v", w)
	}
	// Mako's own order: a part removed from the board holds nothing; one closed in the tracker neither.
	r = core.Record{ID: "PAY-8", After: []string{"PAY-1", "PAY-3"}, Task: core.Task{ID: "PAY-8"}}
	blocker.ReviewState, blocker.Gone = "", "closed"
	if w, _ := waitingFor(r, get(blocker)); w != nil {
		t.Fatalf("waits for %+v", w)
	}
}

// Restack: A is squashed into main; B, started from A's branch, gets only its own commit moved onto main and its MR
// pointed at main, once its agent is done with it.
func TestRestackAfterTheMerge(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	d, f, _ := setup(t)
	ctx := context.Background()
	dir := t.TempDir()
	remote, other, wtA, wtB := filepath.Join(dir, "origin.git"), filepath.Join(dir, "other"), filepath.Join(dir, "a"), filepath.Join(dir, "b")
	put := func(repo, file, body string) {
		os.WriteFile(filepath.Join(repo, file), []byte(body), 0o644)
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-q", "-m", "Change "+file)
		git(t, repo, "push", "-q", "origin", "HEAD")
	}
	out := func(repo string, args ...string) string {
		b, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(b))
	}
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "clone", "-q", remote, other)
	put(other, "a.txt", "one\n")
	git(t, dir, "clone", "-q", remote, wtA)
	git(t, wtA, "checkout", "-q", "-b", "fix/5")
	put(wtA, "b.txt", "mine\n")
	put(wtA, "b.txt", "mine, reviewed\n")
	tip := out(wtA, "rev-parse", "HEAD")
	git(t, dir, "clone", "-q", remote, wtB)
	git(t, wtB, "checkout", "-q", "-b", "fix/6", "origin/fix/5")
	put(wtB, "c.txt", "child\n")
	git(t, wtB, "branch", "-q", "-u", "origin/fix/6")

	a, b := "grp/app#5", "grp/app#6"
	mr := f.base + "/grp/app/-/merge_requests/1"
	f.mrs = append(f.mrs, map[string]any{"iid": 1, "state": "opened", "source_branch": "fix/6", "target_branch": "fix/5", "web_url": mr})
	d.Store.Update(a, func(r *core.Record) {
		r.Status, r.ReviewState, r.Branch = core.Done, "merged", "fix/5"
		r.Task = core.Task{ID: a, TrackerID: "gitlab", Project: "grp/app", Title: "A"}
	})
	d.Store.Update(b, func(r *core.Record) {
		r.Status, r.Worktree, r.Branch, r.PRURL = core.HandedOff, wtB, "fix/6", mr
		r.Task = core.Task{ID: b, TrackerID: "gitlab", Project: "grp/app", Title: "B"}
		r.After, r.StackOn, r.Stack = []string{a}, a, &core.Stack{On: a, Branch: "fix/5", Tip: tip}
	})
	// A is squashed into main.
	git(t, other, "fetch", "-q", "origin")
	git(t, other, "merge", "-q", "--squash", "origin/fix/5")
	git(t, other, "commit", "-q", "-m", "A (squashed)")
	git(t, other, "push", "-q", "origin", "main")

	// Orca's agent still works on B: its branch is its own.
	d.Restack(ctx, time.Now())
	if r := mustGet(t, d, b); r.Stack == nil || f.mrs[0]["target_branch"] != "fix/5" {
		t.Fatalf("moved while the agent works: %+v, MR into %v", r.Stack, f.mrs[0]["target_branch"])
	}

	d.Store.Update(b, func(r *core.Record) { r.Status = core.Done })
	d.Restack(ctx, time.Now())
	if r := mustGet(t, d, b); r.Stack != nil {
		t.Fatalf("still stacked: %+v, logs %+v", r.Stack, d.Logs(b))
	}
	if f.mrs[0]["target_branch"] != "main" {
		t.Fatalf("the MR goes into %v, want main", f.mrs[0]["target_branch"])
	}
	git(t, wtB, "fetch", "-q", "origin")
	if n := out(wtB, "rev-list", "--count", "origin/main..origin/fix/6"); n != "1" {
		t.Fatalf("fix/6 carries %s commits past main, want its own one", n)
	}
	if loggedTimes(d, b, a+" is merged: the branch is moved onto main") != 1 {
		t.Fatalf("logs %+v", d.Logs(b))
	}
	// Its diff and its checks go against main again.
	if _, base, err := d.workOf(b); err != nil || base != "main" {
		t.Fatalf("base %q %v", base, err)
	}
}

// Restack with conflicts: the branch stays as it was and stacked, the reason is said once.
func TestRestackConflict(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	d, _, _ := setup(t)
	ctx := context.Background()
	dir := t.TempDir()
	remote, other, wtB := filepath.Join(dir, "origin.git"), filepath.Join(dir, "other"), filepath.Join(dir, "b")
	put := func(repo, file, body string) {
		os.WriteFile(filepath.Join(repo, file), []byte(body), 0o644)
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-q", "-m", "Change "+file)
		git(t, repo, "push", "-q", "origin", "HEAD")
	}
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "clone", "-q", remote, other)
	put(other, "a.txt", "one\n")
	git(t, dir, "clone", "-q", remote, wtB)
	git(t, wtB, "checkout", "-q", "-b", "fix/6")
	tip, _ := exec.Command("git", "-C", wtB, "rev-parse", "HEAD").Output()
	put(wtB, "a.txt", "child\n")
	put(other, "a.txt", "main moved\n")

	a, b := "grp/app#5", "grp/app#6"
	d.Store.Update(a, func(r *core.Record) {
		r.Status, r.ReviewState = core.Done, "merged"
		r.Task = core.Task{ID: a, TrackerID: "gitlab", Project: "grp/app"}
	})
	d.Store.Update(b, func(r *core.Record) {
		r.Status, r.Worktree, r.Branch = core.Done, wtB, "fix/6"
		r.Task = core.Task{ID: b, TrackerID: "gitlab", Project: "grp/app"}
		r.Stack = &core.Stack{On: a, Branch: "fix/5", Tip: strings.TrimSpace(string(tip))}
	})
	head, _ := exec.Command("git", "-C", wtB, "rev-parse", "HEAD").Output()
	for range 2 {
		d.Restack(ctx, time.Now())
	}
	if r := mustGet(t, d, b); r.Stack == nil {
		t.Fatal("unstacked with conflicts")
	}
	if now, _ := exec.Command("git", "-C", wtB, "rev-parse", "HEAD").Output(); string(now) != string(head) {
		t.Fatal("the branch changed with conflicts")
	}
	if n := loggedTimes(d, b, "could not ask the agent"); n != 1 {
		t.Fatalf("said %d times: %+v", n, d.Logs(b))
	}
}

func mustGet(t *testing.T, d *Dispatcher, id string) core.Record {
	t.Helper()
	r, ok := d.Store.Get(id)
	if !ok {
		t.Fatalf("%s not on the board", id)
	}
	return r
}

// While A is in review, B started from its branch follows it: A gets a commit, B's own commits go onto it (rebased,
// though the settings say merge: a merge would tangle the two branches) and the commit B sits on moves with it.
func TestAutoRebaseFollowsTheStack(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	d, _, _ := setup(t)
	ctx := context.Background()
	cfg := *d.Config()
	cfg.Terminal.AutoRebase = "merge"
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	dir := t.TempDir()
	remote, wtA, wtB := filepath.Join(dir, "origin.git"), filepath.Join(dir, "a"), filepath.Join(dir, "b")
	put := func(repo, file, body string) {
		os.WriteFile(filepath.Join(repo, file), []byte(body), 0o644)
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-q", "-m", "Change "+file)
		git(t, repo, "push", "-q", "origin", "HEAD")
	}
	rev := func(repo, ref string) string {
		b, _ := exec.Command("git", "-C", repo, "rev-parse", ref).Output()
		return strings.TrimSpace(string(b))
	}
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "clone", "-q", remote, wtA)
	put(wtA, "a.txt", "one\n")
	git(t, wtA, "checkout", "-q", "-b", "fix/5")
	put(wtA, "b.txt", "mine\n")
	git(t, dir, "clone", "-q", remote, wtB)
	git(t, wtB, "checkout", "-q", "-b", "fix/6", "origin/fix/5")
	tip := rev(wtB, "HEAD")
	put(wtB, "c.txt", "child\n")
	put(wtA, "b.txt", "mine, after review\n") // A moves on

	a, b := "grp/app#5", "grp/app#6"
	d.Store.Update(a, func(r *core.Record) {
		r.Status, r.PRURL, r.Branch = core.Done, "https://gitlab.example/grp/app/-/merge_requests/1", "fix/5"
		r.Task = core.Task{ID: a, TrackerID: "gitlab", Project: "grp/app"}
	})
	d.Store.Update(b, func(r *core.Record) {
		r.Status, r.Executor, r.Worktree, r.Branch = core.Done, "terminal", wtB, "fix/6"
		r.PRURL = "https://gitlab.example/grp/app/-/merge_requests/2"
		r.Task = core.Task{ID: b, TrackerID: "gitlab", Project: "grp/app"}
		r.Stack = &core.Stack{On: a, Branch: "fix/5", Tip: tip}
	})
	d.mu.Lock()
	d.reviews[b] = reviewLook{at: time.Now(), check: &trackers.ReviewCheck{State: "open"}}
	d.mu.Unlock()
	d.AutoRebase(ctx, time.Now())

	r := mustGet(t, d, b)
	if r.Stack == nil || r.Stack.Tip != rev(wtA, "HEAD") {
		t.Fatalf("stack %+v, want the tip %s; logs %+v", r.Stack, rev(wtA, "HEAD"), d.Logs(b))
	}
	git(t, wtB, "fetch", "-q", "origin")
	if n, _ := exec.Command("git", "-C", wtB, "rev-list", "--merges", "--count", "origin/fix/6").Output(); strings.TrimSpace(string(n)) != "0" {
		t.Fatal("merged instead of rebased")
	}
	if err := exec.Command("git", "-C", wtB, "merge-base", "--is-ancestor", "origin/fix/5", "origin/fix/6").Run(); err != nil {
		t.Fatal("fix/6 is not on fix/5 as it is now")
	}
	if loggedTimes(d, b, "updated from fix/5 (rebase, 1 commit)") != 1 {
		t.Fatalf("logs %+v", d.Logs(b))
	}
}
