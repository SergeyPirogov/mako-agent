package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// The shared worktree of «Запитати про код»: one per project, on its base branch with no branch of its own (a
// detached HEAD), next to the worktrees of its tasks. A new conversation brings it up to date with the base branch,
// unless changes are left in it (by a command the agent ran: it stays as it is until the developer resets it) or
// the agent of another question is at work in it (the code does not change under its feet; idle ones are closed,
// a message resumes them).

// AskWorkspace: the project's shared worktree as the board shows it.
type AskWorkspace struct {
	Dir    string   `json:"dir"`
	Base   string   `json:"base"`
	Exists bool     `json:"exists"`
	Dirty  int      `json:"dirty"`           // uncommitted changes
	Behind int      `json:"behind"`          // the base branch's commits it has not got
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

// prepareAsk: the project's shared worktree, made when there is none; refresh brings it up to date with the base
// branch when nothing keeps it from that. setup: what prepares a worktree just made (its dependencies), as for the
// worktree of a task; "" when it was there already.
func (d *Dispatcher) prepareAsk(p CodeProject, refresh bool) (dir, setup string, err error) {
	askMu.Lock()
	defer askMu.Unlock()
	return d.prepareAskLocked(p, refresh)
}

// prepareAskLocked is prepareAsk under askMu.
func (d *Dispatcher) prepareAskLocked(p CodeProject, refresh bool) (string, string, error) {
	ctx := context.Background()
	dir := askDir(p)
	if !exists(dir) {
		askFetch(ctx, p)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", "", err
		}
		gitx.Run(ctx, p.Path, "worktree", "prune")
		if _, err := gitx.Run(ctx, p.Path, "worktree", "add", "--detach", dir, askRef(ctx, p)); err != nil {
			os.RemoveAll(dir)
			return "", "", errors.New("не вдалося створити worktree для питань: " + err.Error())
		}
		repo := p.repo
		if repo == nil {
			repo = &config.Repo{}
		}
		return dir, setupOf(repo, d.Config().Terminal, dir), nil
	}
	if !refresh {
		return dir, "", nil
	}
	// The fetch changes no file of the worktree: even when it stays as it is, the board can tell how far behind.
	askFetch(ctx, p)
	if files, _ := dirtyFiles(ctx, dir); len(files) > 0 {
		return dir, "", nil
	}
	if d.askWorking(p.Key) {
		return dir, "", nil
	}
	if _, err := gitx.Run(ctx, dir, "checkout", "--detach", askRef(ctx, p)); err != nil {
		return "", "", errors.New("не вдалося оновити worktree для питань: " + err.Error())
	}
	return dir, "", nil
}

// askSetups: the questions whose worktree is being prepared, by id: their agent starts once it is done.
var askSetups sync.Map

type pendingSetup struct{ command, prompt string }

// launchQuestion starts the question's agent, after the worktree's setup when it was just made: in the background,
// the conversation shows the setup running meanwhile.
func (d *Dispatcher) launchQuestion(p CodeProject, t CodeThread, text, setup string) error {
	if setup == "" {
		return d.startQuestion(p, t, text, nil)
	}
	askSetups.Store(t.ID, pendingSetup{command: setup, prompt: text})
	go func() {
		res := d.runSetup(context.Background(), t.ID, threadDir(p, t), setup)
		if res.Failed {
			log.Printf("[%s] підготовка worktree для питань не вдалась, агент стартує все одно:\n%s", t.ID, tailLines(res.Output, 10))
		}
		err := d.startQuestion(p, t, text, res)
		askSetups.Delete(t.ID)
		if err != nil {
			log.Printf("[%s] агент не стартував: %v", t.ID, err)
		}
	}()
	return nil
}

// setupChat: the conversation of a question while its worktree is being prepared.
func setupChat(id string) (chat.Snapshot, bool) {
	v, ok := askSetups.Load(id)
	if !ok {
		return chat.Snapshot{}, false
	}
	s := v.(pendingSetup)
	input, _ := json.Marshal(map[string]string{"command": s.command})
	// A version no session has: the board reads the conversation anew when the agent starts.
	return chat.Snapshot{Version: -2, State: "working", Live: true, Items: []chat.Item{
		{Kind: "user", Text: s.prompt},
		{Kind: "tool", ID: "setup", Tool: "Підготовка", Input: input, Status: "running"},
	}}, true
}

// askWorking: an agent of the project's questions is at work (or waits for an answer) in the shared worktree; the
// idle ones are closed on the way, a message resumes them.
func (d *Dispatcher) askWorking(project string) bool {
	working := false
	for _, t := range d.CodeThreads(project) {
		s := d.Chats.Get(t.ID)
		if s == nil || !s.Running() {
			continue
		}
		if state, _, _ := s.State(); state == "working" || state == "waiting" {
			working = true
			continue
		}
		stopChat(s)
	}
	return working
}

func stopChat(s *chat.Session) {
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
	}
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
	if out, err := gitx.Run(context.Background(), w.Dir, "rev-list", "--count", "HEAD.."+askRef(context.Background(), p)); err == nil {
		w.Behind, _ = strconv.Atoi(out)
	}
	return w, nil
}

// ResetAsk: «Скинути й оновити» — the changes in the shared worktree are thrown away and it is brought up to date.
func (d *Dispatcher) ResetAsk(project string) error {
	p, err := d.codeProject(project)
	if err != nil {
		return err
	}
	// All under one lock: no new question starts its agent between the reset and the update.
	askMu.Lock()
	defer askMu.Unlock()
	d.stopQuestions(project)
	if dir := askDir(p); exists(dir) {
		ctx := context.Background()
		if _, err := gitx.Run(ctx, dir, "reset", "--hard", "--quiet"); err != nil {
			return err
		}
		if _, err := gitx.Run(ctx, dir, "clean", "-fd", "--quiet"); err != nil {
			return err
		}
	}
	_, _, err = d.prepareAskLocked(p, true)
	return err
}

// RemoveAsk: «Видалити» — the shared worktree goes, changes and all; the next question makes it again.
func (d *Dispatcher) RemoveAsk(project string) error {
	p, err := d.codeProject(project)
	if err != nil {
		return err
	}
	askMu.Lock()
	defer askMu.Unlock()
	d.stopQuestions(project)
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
			stopChat(s)
		}
	}
}
