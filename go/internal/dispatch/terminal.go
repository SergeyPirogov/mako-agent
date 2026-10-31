package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// Mako's own terminal, the alternative to Orca: Mako makes the worktree and runs the agent in a terminal the
// board shows. The rest is the same: the agent pushes and opens the MR, Mako follows it.

const terminalExecutor = "terminal"

func onTerminal(r core.Record) bool { return r.Executor == terminalExecutor }

// handTerminal makes the task's worktree and starts Claude in it with the same prompt Orca would get.
func (d *Dispatcher) handTerminal(ctx context.Context, j orca.Job, log func(string)) (*orca.Result, error) {
	cfg := d.Config()
	worktree, err := addWorktree(ctx, j.Repo, j.Task.Slug, j.Branch, log)
	if err != nil {
		return nil, err
	}
	prompt := orca.Prompt(j.Task, j.TrackerName) + "\n\n" + orca.Instructions(cfg.Orca.Instructions, cfg.Orca.MRBy, j)
	if err := d.runClaude(j.Task.ID, worktree, cfg.Terminal, []string{prompt}); err != nil {
		return nil, err
	}
	return &orca.Result{Worktree: worktree, Branch: j.Branch, Note: "термінал Mako, " + worktree}, nil
}

// ResumeTerminal starts Claude again in the task's worktree, continuing its last conversation: after Mako
// restarted or the agent was closed. Without a conversation to continue, it starts the task over there.
func (d *Dispatcher) ResumeTerminal(id string) error {
	r, ok := d.Store.Get(id)
	if !ok || !onTerminal(r) || r.Worktree == "" {
		return fmt.Errorf("задача не в терміналі Mako")
	}
	if s := d.Sessions.Get(id); s != nil && s.Running() {
		return nil
	}
	if !exists(r.Worktree) {
		return fmt.Errorf("worktree %s видалено", r.Worktree)
	}
	args := []string{"--continue"}
	if !orca.HasTranscript(r.Worktree) {
		// No conversation was kept (the agent never got going, or ran without transcripts): start the task over
		// in the same worktree.
		t := d.tracker(r.Task.TrackerID)
		if t == nil {
			return fmt.Errorf("трекер %s прибрано з налаштувань", r.Task.TrackerID)
		}
		repo, err := d.route(t, r.Task)
		if err != nil {
			return err
		}
		cfg := d.Config()
		j := orca.Job{Task: r.Task, TrackerName: t.Name(), Repo: repo, Branch: branchOf(r)}
		args = []string{orca.Prompt(j.Task, j.TrackerName) + "\n\n" + orca.Instructions(cfg.Orca.Instructions, cfg.Orca.MRBy, j)}
	}
	if err := d.runClaude(id, r.Worktree, d.Config().Terminal, args); err != nil {
		return err
	}
	if len(args) == 1 && args[0] == "--continue" {
		d.Log(id, "агента знову запущено в терміналі")
	} else {
		d.Log(id, "розмови агента не збережено: задачу почато знову в тому самому worktree")
	}
	return nil
}

// StopTerminal ends the task's agent; its worktree stays.
func (d *Dispatcher) StopTerminal(id string) {
	if s := d.Sessions.Get(id); s != nil {
		s.Stop()
	}
}

func (d *Dispatcher) runClaude(id, worktree string, t config.Terminal, args []string) error {
	command, err := term.Resolve(t.Command)
	if err != nil {
		return err
	}
	token := randomToken()
	var all []string
	if isClaude(command) {
		settings, err := d.Sessions.ClaudeSettings(id, fmt.Sprintf("%s/hook?id=%s&token=%s", d.BoardURL, escapeQuery(id), token))
		if err != nil {
			return err
		}
		all = append([]string{"--settings", settings}, args...)
		// After the prompt: --allowedTools takes several values and would swallow it otherwise.
		if t.PermissionMode != "" && t.PermissionMode != "default" {
			all = append(all, "--permission-mode", t.PermissionMode)
		}
		if len(t.AllowedTools) > 0 {
			all = append(append(all, "--allowedTools"), t.AllowedTools...)
		}
	} else {
		all = args
	}
	s, err := term.Start(term.Options{
		ID: id, Worktree: worktree, Command: command, Args: all, Token: token,
		Env:     []string{"MAKO_TASK=" + id},
		OnState: func() { d.watchTerminals(time.Now()) },
	})
	if err != nil {
		return fmt.Errorf("агент не запустився: %w", err)
	}
	d.Sessions.Put(s)
	return nil
}

func isClaude(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "claude"
}

func randomToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func escapeQuery(s string) string {
	return strings.NewReplacer("%", "%25", "#", "%23", "&", "%26", "+", "%2B", " ", "%20", "/", "%2F").Replace(s)
}

// addWorktree: <projects>/.worktrees/<repo>/<slug> on a new branch from the base branch, as Orca lays them out.
// A branch that exists already (a retry of the same branch) is checked out as it is.
func addWorktree(ctx context.Context, repo *config.Repo, slug, branch string, log func(string)) (string, error) {
	dir := filepath.Join(filepath.Dir(repo.Path), ".worktrees", filepath.Base(repo.Path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, slug)
	for i := 2; exists(path); i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s-%d", slug, i))
	}
	if _, err := gitx.Run(ctx, repo.Path, "fetch", "origin", repo.BaseBranch); err != nil {
		log("не вдалось оновити " + repo.BaseBranch + ": " + err.Error())
	}
	base := "origin/" + repo.BaseBranch
	if _, err := gitx.Run(ctx, repo.Path, "rev-parse", "--verify", "--quiet", base); err != nil {
		base = repo.BaseBranch
	}
	if _, err := gitx.Run(ctx, repo.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err = gitx.Run(ctx, repo.Path, "worktree", "add", path, branch)
		return path, err
	}
	if _, err := gitx.Run(ctx, repo.Path, "worktree", "add", "--no-track", "-b", branch, path, base); err != nil {
		return "", err
	}
	log("worktree " + path + " на гілці " + branch)
	return path, nil
}

// lookTerminal: the agent of a task in Mako's terminal, from its hooks.
func (d *Dispatcher) lookTerminal(r core.Record, now time.Time) Agent {
	a := Agent{Name: "claude", At: now.UnixMilli(), Terminal: true}
	s := d.Sessions.Get(r.ID)
	if s == nil {
		a.State, a.Reason = "stopped", "сесії немає: Mako перезапускали"
	} else {
		st := s.State()
		a.Since = st.Since.UnixMilli()
		switch st.State {
		case "exited":
			a.State, a.Reason = "stopped", "агент вийшов"
		case "waiting":
			a.State, a.Reason = "waiting", st.Reason
		case "done":
			a.State = "done"
		default:
			a.State, a.Tool = "working", st.Tool
		}
		a.Live = st.State != "exited"
	}
	if d.LastMessage != nil && r.Worktree != "" {
		a.Message = d.LastMessage(r.Worktree)
	}
	return a
}

// watchTerminals updates the agents in Mako's terminal right away: their hooks report every change.
func (d *Dispatcher) watchTerminals(now time.Time) {
	for _, r := range d.Store.All() {
		if r.Status == core.HandedOff && onTerminal(r) {
			d.setAgent(r, d.lookTerminal(r, now), now)
		}
	}
}

// Changes: what the task's branch changed against its base branch, committed or not.
func (d *Dispatcher) Changes(ctx context.Context, id string) (*gitx.ChangeSet, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return nil, err
	}
	return gitx.Changes(ctx, wt, base)
}

// FileDiff: one changed file of the task.
func (d *Dispatcher) FileDiff(ctx context.Context, id, path string) (string, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return "", err
	}
	return gitx.FileDiff(ctx, wt, base, path)
}

// workOf: the task's worktree and the branch it goes into.
func (d *Dispatcher) workOf(id string) (worktree, baseBranch string, err error) {
	r, ok := d.Store.Get(id)
	if !ok || r.Worktree == "" || !exists(r.Worktree) {
		return "", "", fmt.Errorf("у задачі немає worktree")
	}
	if t := d.tracker(r.Task.TrackerID); t != nil {
		if repo, err := d.route(t, r.Task); err == nil && repo.BaseBranch != "" {
			return r.Worktree, repo.BaseBranch, nil
		}
	}
	return r.Worktree, gitx.DetectBaseBranch(r.Worktree), nil
}
