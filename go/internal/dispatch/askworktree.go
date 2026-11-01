package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// The shared worktree of «Запитати про код»: one per project, on its base branch with no branch of its own (a
// detached HEAD), next to the worktrees of its tasks. A new conversation brings it up to date with the base branch;
// changes left in it (by a command the agent ran) keep it as it is until the developer resets it.

// AskWorkspace: the project's shared worktree as the board shows it.
type AskWorkspace struct {
	Dir    string   `json:"dir"`
	Base   string   `json:"base"`
	Exists bool     `json:"exists"`
	Dirty  int      `json:"dirty"`           // uncommitted changes
	Files  []string `json:"files,omitempty"` // the first of them
}

// askDir: where the project's shared worktree is, as addWorktree lays out the worktrees of tasks.
func askDir(p CodeProject) string {
	dir := ""
	if p.repo != nil {
		dir = p.repo.WorktreesDir
	}
	if dir == "" {
		dir = filepath.Join(filepath.Dir(p.Path), ".worktrees", filepath.Base(p.Path))
	}
	return filepath.Join(dir, askSlug)
}

func askBase(p CodeProject) string {
	if p.repo != nil && p.repo.BaseBranch != "" {
		return p.repo.BaseBranch
	}
	if b := gitx.DetectBaseBranch(p.Path); b != "" {
		return b
	}
	return "main"
}

// askFetch brings the base branch up to date; offline the worktree starts from what is there.
func askFetch(ctx context.Context, p CodeProject) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	gitx.Run(ctx, p.Path, "fetch", "origin", askBase(p))
}

// askRef: the base branch as the remote has it, else the local one.
func askRef(ctx context.Context, p CodeProject) string {
	ref := "origin/" + askBase(p)
	if _, err := gitx.Run(ctx, p.Path, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return askBase(p)
	}
	return ref
}

// prepareAsk: the project's shared worktree, made when there is none; refresh brings a clean one up to date with
// the base branch. dirty: its uncommitted changes, which keep it from being brought up to date.
func (d *Dispatcher) prepareAsk(p CodeProject, refresh bool) (dir string, dirty int, err error) {
	askMu.Lock()
	defer askMu.Unlock()
	ctx := context.Background()
	dir = askDir(p)
	if !exists(dir) {
		askFetch(ctx, p)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", 0, err
		}
		gitx.Run(ctx, p.Path, "worktree", "prune")
		if _, err := gitx.Run(ctx, p.Path, "worktree", "add", "--detach", dir, askRef(ctx, p)); err != nil {
			os.RemoveAll(dir)
			return "", 0, errors.New("не вдалося створити worktree для питань: " + err.Error())
		}
		return dir, 0, nil
	}
	files, _ := dirtyFiles(ctx, dir)
	if len(files) > 0 || !refresh {
		return dir, len(files), nil
	}
	askFetch(ctx, p)
	if _, err := gitx.Run(ctx, dir, "checkout", "--detach", askRef(ctx, p)); err != nil {
		return "", 0, errors.New("не вдалося оновити worktree для питань: " + err.Error())
	}
	return dir, 0, nil
}

// dirtyFiles: the worktree's uncommitted changes, their paths.
func dirtyFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := gitx.Run(ctx, dir, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if i := strings.IndexByte(l, ' '); i >= 0 {
			l = strings.TrimSpace(l[i+1:])
		}
		files = append(files, l)
	}
	return files, nil
}

// AskStatus: the project's shared worktree, whether it is there and what is changed in it.
func (d *Dispatcher) AskStatus(project string) (AskWorkspace, error) {
	p, err := d.codeProject(project)
	if err != nil {
		return AskWorkspace{}, err
	}
	w := AskWorkspace{Dir: askDir(p), Base: askBase(p)}
	if !exists(w.Dir) {
		return w, nil
	}
	w.Exists = true
	files, _ := dirtyFiles(context.Background(), w.Dir)
	w.Dirty = len(files)
	if len(files) > 20 {
		files = files[:20]
	}
	w.Files = files
	return w, nil
}

// ResetAsk: «Скинути й оновити» — the changes in the shared worktree are thrown away and it is brought up to date.
func (d *Dispatcher) ResetAsk(project string) error {
	p, err := d.codeProject(project)
	if err != nil {
		return err
	}
	d.stopQuestions(project)
	askMu.Lock()
	dir := askDir(p)
	if exists(dir) {
		ctx := context.Background()
		if _, err := gitx.Run(ctx, dir, "reset", "--hard", "--quiet"); err != nil {
			askMu.Unlock()
			return err
		}
		if _, err := gitx.Run(ctx, dir, "clean", "-fd", "--quiet"); err != nil {
			askMu.Unlock()
			return err
		}
	}
	askMu.Unlock()
	_, _, err = d.prepareAsk(p, true)
	return err
}

// RemoveAsk: «Видалити» — the shared worktree goes, changes and all; the next question makes it again.
func (d *Dispatcher) RemoveAsk(project string) error {
	p, err := d.codeProject(project)
	if err != nil {
		return err
	}
	d.stopQuestions(project)
	askMu.Lock()
	defer askMu.Unlock()
	dir := askDir(p)
	if !exists(dir) {
		return nil
	}
	if err := gitx.DropWorktree(context.Background(), p.Path, dir, ""); err != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return err
		}
		gitx.Run(context.Background(), p.Path, "worktree", "prune")
	}
	return nil
}

// stopQuestions: the agents answering the project's questions stop, before their worktree changes under them.
func (d *Dispatcher) stopQuestions(project string) {
	for _, t := range d.CodeThreads(project) {
		if s := d.Chats.Get(t.ID); s != nil && s.Running() {
			s.Stop()
			select {
			case <-s.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}
