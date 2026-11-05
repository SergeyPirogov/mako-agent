package dispatch

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// A merged or archived task leaves nothing behind on the Mac: its worktree, its local branch, and the
// branches (and worktrees) of its earlier attempts go. A worktree with uncommitted work stays until you say so,
// and so does everything of a task whose agent still works.

// Cleanup: what one cleaning took away and what it left.
type Cleanup struct {
	Worktrees int `json:"worktrees"`
	Branches  int `json:"branches"`
	// Kept: uncommitted files that kept a worktree; KeptTasks: the tasks they belong to.
	Kept      int      `json:"kept"`
	KeptTasks []string `json:"keptTasks,omitempty"`
}

func (c *Cleanup) add(o Cleanup) {
	c.Worktrees += o.Worktrees
	c.Branches += o.Branches
	c.Kept += o.Kept
	c.KeptTasks = append(c.KeptTasks, o.KeptTasks...)
}

// mrOpen: the task is done and its MR waits for review or merge; the worktree is what «Fix CI», resume and
// the auto-fix need, so it stays whatever the board does with the card.
func mrOpen(r core.Record) bool {
	return r.Status == core.Done && r.PRURL != "" && r.ReviewState == ""
}

// cleanable: the task is over for good: merged, or taken off the board (with no open MR behind it).
func cleanable(r core.Record) bool {
	if mrOpen(r) {
		return false
	}
	return r.ReviewState == "merged" || r.ArchivedAt != 0
}

// agentBusy: the task's agent is still at it, in Mako's terminal or in Orca.
func (d *Dispatcher) agentBusy(r core.Record) bool {
	if onTerminal(r) {
		return !d.agentIdle(r.ID)
	}
	d.mu.Lock()
	a := d.agents[r.ID]
	d.mu.Unlock()
	if a != nil && (a.State == "working" || a.State == "waiting") {
		return true
	}
	// WatchAgents follows only the tasks in work; an agent fixing a review's remarks on a task past its MR
	// is asked in Orca itself. What cannot be told counts as busy: a worktree does not come back.
	if mrOpen(r) && d.Term != nil && (r.Worktree != "" || branchOf(r) != "") {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		o := d.Config().Orca
		ws, err := d.Term.Workspaces(ctx, o)
		if err != nil {
			return true
		}
		if w := orca.FindWorkspace(ws, r.Worktree, branchOf(r)); w != nil {
			st := d.look(ctx, o, r, ws, nil, time.Now()).State
			return st == "working" || st == "waiting"
		}
	}
	return false
}

// CleanTask takes away the task's worktree, its local branch and those of its earlier attempts; force takes
// a worktree with uncommitted work too. Without force such a worktree stays and the task asks about it
// (WorktreeKept).
func (d *Dispatcher) CleanTask(ctx context.Context, id string, force bool) (Cleanup, error) {
	var c Cleanup
	r, ok := d.Store.Get(id)
	if !ok {
		return c, fmt.Errorf("no such task")
	}
	switch r.Status {
	case core.Asking, core.Snoozed, core.Queued, core.HandedOff:
		return c, fmt.Errorf("the task is still in progress")
	}
	if d.busyOrStarting(r) {
		return c, fmt.Errorf("the agent is still working")
	}
	repo := d.repoOf(r)
	if repo == nil || !exists(repo.Path) {
		return c, nil
	}
	// What the branch changed, while its worktree is still there to tell: the finished card shows it.
	if cs, err := d.Changes(ctx, id); err == nil && (len(cs.Files) > 0 || len(cs.Commits) > 0) {
		st := changeStat(cs)
		d.Store.Update(id, func(rec *core.Record) { rec.Changed = st })
	}
	wts, err := gitx.Worktrees(ctx, repo.Path)
	if err != nil {
		return c, err
	}

	branches := attemptBranches(r, repo)
	mine, had := map[string]bool{}, map[string]bool{}
	for _, b := range branches {
		mine[b], had[b] = true, gitx.HasBranch(ctx, repo.Path, b)
	}
	stopped := false
	busy := map[string]bool{} // branches still checked out somewhere
	for _, w := range wts {
		if !samePath(w.Path, r.Worktree) && !mine[w.Branch] {
			busy[w.Branch] = true
			continue
		}
		n, err := gitx.Dirty(ctx, w.Path)
		if err == nil {
			n += gitx.Unpushed(ctx, w.Path) // commits the remote branch lacks would go with the local one
		}
		if err != nil || (n > 0 && !force) {
			c.Kept += n
			busy[w.Branch] = true
			continue
		}
		if onTerminal(r) {
			if !stopped && (d.Sessions.Get(id) != nil || d.Chats.Get(id) != nil) {
				d.StopTerminal(id)
				time.Sleep(500 * time.Millisecond) // the agent lets go of the folder
			}
			stopped = true
			err = gitx.DropWorktree(ctx, repo.Path, w.Path, w.Branch)
		} else {
			err = d.RemoveWorkspace(ctx, repo.Path, w.Path, w.Branch)
		}
		if err != nil {
			d.Fault(id, "worktree kept", err)
			busy[w.Branch] = true
			continue
		}
		c.Worktrees++
	}
	for _, b := range branches {
		if !had[b] {
			continue
		}
		if !busy[b] && gitx.HasBranch(ctx, repo.Path, b) {
			gitx.DeleteBranch(ctx, repo.Path, b)
		}
		if !gitx.HasBranch(ctx, repo.Path, b) {
			c.Branches++
		}
	}

	if c.Kept > 0 {
		c.KeptTasks = []string{id}
	}
	d.Store.Update(id, func(rec *core.Record) { rec.WorktreeKept = c.Kept })
	switch {
	case c.Kept > 0:
		d.Log(id, fmt.Sprintf("worktree kept: %d uncommitted changes or unpushed commits", c.Kept))
	case c.Worktrees > 0 || c.Branches > 0:
		d.Log(id, fmt.Sprintf("removed worktrees: %d, local branches: %d", c.Worktrees, c.Branches))
	}
	return c, nil
}

// KeepWorktree: you said to leave the task's worktree with its uncommitted work; Mako stops asking.
func (d *Dispatcher) KeepWorktree(id string) {
	d.Store.Update(id, func(r *core.Record) { r.WorktreeKept = 0 })
	d.Log(id, "worktree left in place")
}

// cleanUp: a task that is over cleans after itself without asking, unless it is not quite over.
func (d *Dispatcher) cleanUp(ctx context.Context, id string) {
	if r, ok := d.Store.Get(id); !ok || !cleanable(r) {
		return
	}
	if _, err := d.CleanTask(ctx, id, false); err != nil {
		d.Log(id, "worktree not removed: "+err.Error())
	}
}

// CleanMerged: «Прибрати змерджені» in the settings: every merged or archived task's leftovers.
func (d *Dispatcher) CleanMerged(ctx context.Context) Cleanup {
	var all Cleanup
	for _, r := range d.Store.All() {
		if !cleanable(r) {
			continue
		}
		if c, err := d.CleanTask(ctx, r.ID, false); err == nil {
			all.add(c)
		}
	}
	return all
}

// attemptBranches: the task's branch now and those of its earlier attempts (-try2, …).
func attemptBranches(r core.Record, repo *config.Repo) []string {
	seen := map[string]bool{}
	var out []string
	add := func(b string) {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	add(r.Branch)
	add(r.BranchName)
	for k := 1; k < r.Attempt; k++ {
		add(gitx.BranchName(repo.BranchPattern, r.Task, k))
	}
	return out
}

// repoOf: the repository the task was done in, nil when it is not configured any more.
func (d *Dispatcher) repoOf(r core.Record) *config.Repo {
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return nil
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return nil
	}
	return repo
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// WorktreeUse: how much of the disk one project's worktrees take, for the settings.
type WorktreeUse struct {
	Repo      string         `json:"repo"`
	Path      string         `json:"path"`
	Base      string         `json:"base,omitempty"` // the base branch ahead/behind are counted against
	Bytes     int64          `json:"bytes"`
	Worktrees []WorktreeSize `json:"worktrees"`
	// Cleanable: the worktrees of merged or archived tasks, which «Прибрати змерджені» takes away.
	Cleanable      int   `json:"cleanable"`
	CleanableBytes int64 `json:"cleanableBytes"`
}

// WorktreeSize: one worktree with what the settings show about it.
type WorktreeSize struct {
	gitx.Worktree
	Bytes     int64  `json:"bytes"`
	Task      string `json:"task,omitempty"`
	Cleanable bool   `json:"cleanable,omitempty"`

	Dirty           int    `json:"dirty"`    // files changed and not committed
	Unpushed        int    `json:"unpushed"` // commits origin/<branch> does not have
	HasRemote       bool   `json:"hasRemote"`
	Ahead           int    `json:"ahead"` // against the repository's base branch
	Behind          int    `json:"behind"`
	LastCommitAt    string `json:"lastCommit,omitempty"`
	LastCommitTitle string `json:"lastCommitTitle,omitempty"`
	ModifiedAt      string `json:"modifiedAt,omitempty"` // the newest file in the folder

	// Of the task the worktree belongs to.
	TaskStatus  core.Status `json:"taskStatus,omitempty"`
	ReviewState string      `json:"reviewState,omitempty"`
	Executor    string      `json:"executor,omitempty"`
	AgentBusy   bool        `json:"agentBusy,omitempty"`
}

// metaJobs: git processes running at once while the settings read the worktrees.
const metaJobs = 4

// metaTimeout: how long one worktree's size and git state may take; a slower one shows what was read by then.
const metaTimeout = 20 * time.Second

// stopWait: how long a stopped agent has to let go of its worktree.
const stopWait = 5 * time.Second

// configuredRepos: every configured repository that is on the disk, once.
func (d *Dispatcher) configuredRepos() []*config.Repo {
	// The projects are read under d.mu, as ensureProject adds to them; the disk is looked at after it.
	var all []*config.Repo
	d.mu.Lock()
	for _, t := range d.Config().Trackers {
		for _, p := range t.Projects {
			for _, repo := range p.Repos {
				all = append(all, repo)
			}
		}
	}
	d.mu.Unlock()
	seen := map[string]bool{}
	var out []*config.Repo
	for _, repo := range all {
		if seen[repo.Path] || !exists(repo.Path) {
			continue
		}
		seen[repo.Path] = true
		out = append(out, repo)
	}
	return out
}

// taskOf: the task the worktree belongs to, by its folder or by the branch of one of its attempts.
func taskOf(records []core.Record, repo *config.Repo, w gitx.Worktree) (core.Record, bool) {
	for _, r := range records {
		if r.Repo != "" && r.Repo != repo.Name {
			continue
		}
		if samePath(w.Path, r.Worktree) || (w.Branch != "" && slices.Contains(attemptBranches(r, repo), w.Branch)) {
			return r, true
		}
	}
	return core.Record{}, false
}

// busyOrStarting: the task's agent works, or its start is under way.
func (d *Dispatcher) busyOrStarting(r core.Record) bool {
	d.mu.Lock()
	starting := d.starting[r.ID]
	d.mu.Unlock()
	return starting || d.agentBusy(r)
}

// WorktreeUsage: the worktrees of every configured repository, their size and state, the biggest project first.
func (d *Dispatcher) WorktreeUsage(ctx context.Context) []WorktreeUse {
	records := d.Store.All()
	out := []WorktreeUse{}
	sem := make(chan struct{}, metaJobs)
	var wg sync.WaitGroup
	for _, repo := range d.configuredRepos() {
		wts, err := gitx.Worktrees(ctx, repo.Path)
		if err != nil || len(wts) == 0 {
			continue
		}
		u := WorktreeUse{Repo: repo.Name, Path: repo.Path, Base: repo.BaseBranch, Worktrees: make([]WorktreeSize, len(wts))}
		for i, w := range wts {
			s := &u.Worktrees[i]
			s.Worktree = w
			if r, ok := taskOf(records, repo, w); ok {
				s.Task, s.Cleanable = r.ID, cleanable(r)
				s.TaskStatus, s.ReviewState, s.Executor = r.Status, r.ReviewState, r.Executor
				s.AgentBusy = d.busyOrStarting(r)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				mctx, cancel := context.WithTimeout(ctx, metaTimeout)
				defer cancel()
				fillMeta(mctx, s, repo.BaseBranch)
			}()
		}
		out = append(out, u)
	}
	wg.Wait()
	for i := range out {
		u := &out[i]
		for _, s := range u.Worktrees {
			u.Bytes += s.Bytes
			if s.Cleanable {
				u.Cleanable++
				u.CleanableBytes += s.Bytes
			}
		}
		sort.Slice(u.Worktrees, func(i, j int) bool { return u.Worktrees[i].Bytes > u.Worktrees[j].Bytes })
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// fillMeta reads the size and the git state of a worktree; what git cannot tell stays empty.
func fillMeta(ctx context.Context, s *WorktreeSize, base string) {
	var mod time.Time
	s.Bytes, mod = gitx.DirStat(ctx, s.Path)
	if !mod.IsZero() {
		s.ModifiedAt = mod.UTC().Format(time.RFC3339)
	}
	if s.Prunable || !exists(s.Path) {
		return // the folder is gone: git has nothing to say about it
	}
	if n, err := gitx.Dirty(ctx, s.Path); err == nil {
		s.Dirty = n
	}
	s.Unpushed = gitx.Unpushed(ctx, s.Path)
	s.HasRemote = gitx.HasRemoteBranch(ctx, s.Path, s.Branch)
	if base != "" {
		s.Ahead, s.Behind, _ = gitx.AheadBehind(ctx, s.Path, base)
	}
	if at, title, err := gitx.LastCommit(ctx, s.Path); err == nil {
		s.LastCommitAt, s.LastCommitTitle = at.UTC().Format(time.RFC3339), title
	}
}

// WorktreeRef names a worktree to remove: the project it is of and its folder.
type WorktreeRef struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
}

// KeptWorktree: a worktree left because of what it holds.
type KeptWorktree struct {
	Path     string `json:"path"`
	Dirty    int    `json:"dirty"`
	Unpushed int    `json:"unpushed"`
}

type WorktreeError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// RemoveResult: what «Видалити вибрані» took away and what it left.
type RemoveResult struct {
	Removed  int             `json:"removed"`
	Branches int             `json:"branches"`
	Kept     []KeptWorktree  `json:"kept"`
	Errors   []WorktreeError `json:"errors"`
}

// RemoveWorktrees removes the chosen worktrees, Mako's or not. Only a folder `git worktree list` gives for the
// named repository goes (never the main checkout); a task whose agent works keeps its worktree; one with
// uncommitted or unpushed work is listed in Kept and stays unless force.
func (d *Dispatcher) RemoveWorktrees(ctx context.Context, items []WorktreeRef, force bool) RemoveResult {
	res := RemoveResult{Kept: []KeptWorktree{}, Errors: []WorktreeError{}}
	fail := func(path string, err error) {
		res.Errors = append(res.Errors, WorktreeError{Path: path, Error: err.Error()})
	}
	repos := map[string]*config.Repo{}
	for _, repo := range d.configuredRepos() {
		repos[repo.Name] = repo
	}
	records := d.Store.All()
	for _, it := range items {
		repo := repos[it.Repo]
		if repo == nil {
			fail(it.Path, fmt.Errorf("unknown project %q", it.Repo))
			continue
		}
		// The list is read for each: the removal before it changed it.
		wts, err := gitx.Worktrees(ctx, repo.Path)
		if err != nil {
			fail(it.Path, err)
			continue
		}
		i := slices.IndexFunc(wts, func(w gitx.Worktree) bool { return samePath(w.Path, it.Path) })
		if i < 0 {
			fail(it.Path, fmt.Errorf("not a worktree of %s", repo.Name))
			continue
		}
		w := wts[i]
		rec, mako := taskOf(records, repo, w)
		if mako && d.busyOrStarting(rec) {
			fail(w.Path, fmt.Errorf("the agent of %s is still working", rec.ID))
			continue
		}
		base := repo.BaseBranch
		if base == "" {
			base = gitx.DetectBaseBranch(repo.Path)
		}
		if exists(w.Path) && !w.Prunable {
			dirty, derr := gitx.Dirty(ctx, w.Path)
			unsaved := 0
			if derr == nil {
				unsaved, derr = gitx.UnsavedCommits(ctx, w.Path, w.Detached, base)
			}
			if derr != nil && !force {
				fail(w.Path, derr)
				continue
			}
			if (dirty > 0 || unsaved > 0) && !force {
				res.Kept = append(res.Kept, KeptWorktree{Path: w.Path, Dirty: dirty, Unpushed: unsaved})
				continue
			}
		}
		// The branch goes with the worktree, unless another worktree has it checked out or it is the
		// repository's base branch (-D would take it without asking).
		branch := w.Branch
		for j, o := range wts {
			if j != i && o.Branch == branch {
				branch = ""
			}
		}
		if branch == base {
			branch = ""
		}
		if mako && !onTerminal(rec) {
			err = d.RemoveWorkspace(ctx, repo.Path, w.Path, branch)
		} else {
			if mako {
				d.stopAndWait(rec.ID)
			}
			err = dropListedWorktree(ctx, repo.Path, w, branch)
		}
		if err != nil {
			if mako {
				d.Fault(rec.ID, "worktree kept", err)
			}
			fail(w.Path, err)
			continue
		}
		res.Removed++
		if branch != "" && !gitx.HasBranch(ctx, repo.Path, branch) {
			res.Branches++
		}
		if mako {
			d.Store.Update(rec.ID, func(r *core.Record) { r.WorktreeKept = 0 })
			d.Log(rec.ID, "worktree removed from the settings: "+w.Path)
		}
	}
	return res
}

// stopAndWait stops the task's terminal agent and waits until it is gone, so that it has let go of the folder.
func (d *Dispatcher) stopAndWait(id string) {
	if !d.Live(id) {
		return
	}
	d.StopTerminal(id)
	for end := time.Now().Add(stopWait); d.Live(id) && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
}

// dropListedWorktree removes the worktree and its branch. A folder that is already gone (prunable) leaves only what
// git keeps about it, which `git worktree prune` clears.
func dropListedWorktree(ctx context.Context, repo string, w gitx.Worktree, branch string) error {
	if !w.Prunable && exists(w.Path) {
		return gitx.DropWorktree(ctx, repo, w.Path, branch)
	}
	if _, err := gitx.Run(ctx, repo, "worktree", "prune"); err != nil {
		return err
	}
	if branch != "" {
		gitx.DeleteBranch(ctx, repo, branch)
	}
	return nil
}

// OpenWorktreePath shows a worktree of a configured repository in Finder.
func (d *Dispatcher) OpenWorktreePath(ctx context.Context, repoName, path string) error {
	for _, repo := range d.configuredRepos() {
		if repo.Name != repoName {
			continue
		}
		wts, err := gitx.Worktrees(ctx, repo.Path)
		if err != nil {
			return err
		}
		for _, w := range wts {
			if samePath(w.Path, path) && exists(w.Path) {
				return exec.Command("open", w.Path).Start()
			}
		}
	}
	return fmt.Errorf("not a worktree of %s", repoName)
}

// changeStat: the size of a change set.
func changeStat(cs *gitx.ChangeSet) *core.ChangeStat {
	st := &core.ChangeStat{Files: len(cs.Files), Commits: len(cs.Commits)}
	for _, f := range cs.Files {
		st.Added += f.Additions
		st.Removed += f.Deletions
	}
	return st
}
