// Package orca hands a task to Orca (onorca.dev): Orca creates the worktree and starts the agent with the prompt.
package orca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Job is one hand-off.
type Job struct {
	Task        core.Task
	TrackerName string
	Repo        *config.Repo
	Branch      string
	// Model for the agent, "" for its default; RetryNote and LastAttempt: a new attempt's «what to do differently».
	Model       string
	RetryNote   string
	LastAttempt string
}

// Result: where Orca put the work. Branch may differ from Job.Branch: Orca suffixes a taken name.
type Result struct {
	Worktree string
	Branch   string
	Note     string
}

// Hand creates the Orca workspace with the agent and the prompt.
func Hand(ctx context.Context, o config.Orca, j Job, log func(string)) (*Result, error) {
	command, err := Resolve(o.Command)
	if err != nil {
		return nil, err
	}
	agent := orStr(o.Agent, "claude")
	base := []string{
		"worktree", "create",
		"--repo", "path:" + j.Repo.Path,
		"--name", j.Task.Slug,
		"--branch", j.Branch,
		"--base-branch", j.Repo.BaseBranch,
		"--agent", agent,
		"--prompt", FullPrompt(j, o),
		"--setup", orStr(o.Setup, "inherit"),
		"--no-parent",
		"--json",
	}
	if j.Model != "" {
		base = append(base, "--model", j.Model)
	}
	base = append(base, o.ExtraArgs...)
	withLink := base
	if o.LinkIssue && j.Task.URL != "" {
		withLink = append(append([]string{}, base...), "--reference", j.Task.URL)
	}
	if err := ensureRunning(ctx, command, j.Repo.Path, log); err != nil {
		return nil, err
	}
	out, ok := run(ctx, command, j.Repo.Path, withLink...)
	var fail *failure
	if !ok {
		fail = parseFailure(out)
	}
	// Only a complaint about the issue link is worth a second try without it: any other failure may already
	// have started an agent, and launching again would make a duplicate workspace.
	if fail != nil && len(withLink) != len(base) && fail.Code != unknownOutcome && linkComplaint.MatchString(fail.Message) {
		out, ok = run(ctx, command, j.Repo.Path, base...)
		fail = nil
		if !ok {
			fail = parseFailure(out)
		}
	}
	if fail != nil && fail.Code == unknownOutcome {
		// Orca lost the reply mid-launch: the agent may be running. Replay the way Orca says is safe, then look
		// for the workspace before calling it a failure.
		if fail.Data.RetrySafe && len(fail.Data.RecoveryCommand) > 1 {
			if out2, ok2 := run(ctx, fail.Data.RecoveryCommand[0], j.Repo.Path, fail.Data.RecoveryCommand[1:]...); ok2 {
				out, fail = out2, nil
			}
		}
		if fail != nil {
			if path, branch := findWorkspace(ctx, command, j); branch != "" {
				mark(ctx, command, j.Repo.Path, path, branch, "in-progress")
				return &Result{Worktree: path, Branch: branch, Note: "Orca не підтвердила, що промпт дійшов до агента, перевірте його в Orca"}, nil
			}
		}
	}
	if fail != nil {
		return nil, errors.New(fail.explain())
	}
	res := &Result{Branch: j.Branch, Note: fmt.Sprintf("Orca workspace %s, агент %s", j.Task.Slug, agent)}
	var reply struct {
		Path     string `json:"path"`
		Branch   string `json:"branch"`
		Worktree *struct {
			Path   string `json:"path"`
			Branch string `json:"branch"`
		} `json:"worktree"`
		Result *struct {
			Worktree *struct {
				Path   string `json:"path"`
				Branch string `json:"branch"`
			} `json:"worktree"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(out), &reply) == nil {
		path, branch := reply.Path, reply.Branch
		if reply.Worktree != nil {
			path, branch = reply.Worktree.Path, reply.Worktree.Branch
		} else if reply.Result != nil && reply.Result.Worktree != nil {
			path, branch = reply.Result.Worktree.Path, reply.Result.Worktree.Branch
		}
		res.Worktree = path
		if branch != "" {
			res.Branch = strings.TrimPrefix(branch, "refs/heads/")
		}
	}
	mark(ctx, command, j.Repo.Path, res.Worktree, res.Branch, "in-progress")
	return res, nil
}

// Remove takes the task's workspace out of Orca: its agent stops, the worktree and its local branch go.
func Remove(ctx context.Context, o config.Orca, repoPath, worktree, branch string) error {
	command, err := Resolve(o.Command)
	if err != nil {
		return err
	}
	sel := "branch:" + branch
	if worktree != "" {
		sel = "path:" + worktree
	}
	if out, ok := run(ctx, command, repoPath, "worktree", "rm", "--worktree", sel, "--force", "--json"); !ok {
		return fmt.Errorf("orca worktree rm: %s", parseFailure(out).Message)
	}
	return nil
}

// Mark moves the task's workspace to a column of Orca's board ("in-progress", "in-review", "done").
func Mark(ctx context.Context, o config.Orca, repoPath, worktree, branch, status string) {
	if command, err := Resolve(o.Command); err == nil {
		mark(ctx, command, repoPath, worktree, branch, status)
	}
}

func mark(ctx context.Context, command, repoPath, worktree, branch, status string) {
	sel := "branch:" + branch
	if worktree != "" {
		sel = "path:" + worktree
	}
	run(ctx, command, repoPath, "worktree", "set", "--worktree", sel, "--workspace-status", status, "--json")
}

var linkComplaint = regexp.MustCompile(`(?i)reference|link|url|issue source`)

// Resolve finds the Orca CLI: the configured one, PATH, or inside Orca.app (the CLI reaches PATH only after
// Orca → Settings → General → Orca CLI). On Linux it is orca-ide: GNOME's screen reader owns `orca`.
func Resolve(configured string) (string, error) {
	if configured != "" {
		if p, err := exec.LookPath(config.Expand(configured)); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("Orca CLI не знайдено за шляхом «%s». Перевірте команду Orca в налаштуваннях", configured)
	}
	name := "orca"
	if runtime.GOOS == "linux" {
		name = "orca-ide"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		for _, apps := range []string{"/Applications", filepath.Join(home, "Applications")} {
			p := filepath.Join(apps, "Orca.app/Contents/Resources/bin/orca")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("Orca CLI не знайдено. Встановіть Orca (onorca.dev) і зареєструйте CLI: Orca → Settings → General → Orca CLI, " +
		"або вкажіть повний шлях у налаштуваннях Mako (orca.command)")
}

// ensureRunning: the CLI talks to the Orca app, so a closed app fails every hand-off. On macOS Mako opens
// Orca and waits up to a minute for the CLI to answer; elsewhere it asks to open it.
func ensureRunning(ctx context.Context, command, repoPath string, log func(string)) error {
	answers := func() bool {
		_, ok := run(ctx, command, repoPath, "worktree", "list", "--repo", "path:"+repoPath, "--json")
		return ok
	}
	if answers() || appRunning() {
		return nil
	}
	if runtime.GOOS != "darwin" {
		return errors.New("Orca не запущена. Відкрийте Orca і натисніть «Повторити»")
	}
	log("Orca не запущена: відкриваю її і чекаю")
	if err := exec.Command("open", "-a", "Orca").Run(); err != nil {
		return errors.New("Orca не запущена, і Mako не зміг її відкрити. Відкрийте Orca і натисніть «Повторити»")
	}
	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if answers() {
			return nil
		}
	}
	return nil // up but not answering for this repo: the hand-off itself says what is wrong
}

func appRunning() bool {
	name := "orca-ide"
	if runtime.GOOS == "darwin" {
		name = "Orca"
	}
	return exec.Command("pgrep", "-x", name).Run() == nil
}

func run(ctx context.Context, command, dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

const unknownOutcome = "agent_session_operation_unknown"

type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Cause           string   `json:"cause"`
		NextSteps       []string `json:"nextSteps"`
		RecoveryCommand []string `json:"recoveryCommand"`
		RetrySafe       bool     `json:"retrySafe"`
	} `json:"data"`
}

// parseFailure reads Orca's --json error ({ok:false, error:{code, message, data}}), or plain text.
func parseFailure(out string) *failure {
	text := strings.TrimSpace(out)
	if i := strings.Index(text, "{"); i >= 0 {
		var reply struct {
			Error *failure `json:"error"`
		}
		if json.Unmarshal([]byte(text[i:]), &reply) == nil && reply.Error != nil {
			return reply.Error
		}
	}
	if text == "" {
		text = "orca завершився з помилкою"
	}
	return &failure{Message: text}
}

var errorCode = regexp.MustCompile(`^[a-z_]+$`)

func (f *failure) explain() string {
	var lines []string
	if f.Code == unknownOutcome {
		lines = append(lines, "Orca не змогла підтвердити запуск агента, а workspace задачі не знайдено.")
	} else if f.Code != "" {
		lines = append(lines, fmt.Sprintf("Orca не створила workspace (%s): %s", f.Code, f.Message))
	} else {
		lines = append(lines, "Orca не створила workspace: "+f.Message)
	}
	if c := strings.TrimSpace(f.Data.Cause); c != "" && !errorCode.MatchString(c) {
		lines = append(lines, "Причина від Orca: "+c)
	}
	for _, s := range f.Data.NextSteps {
		lines = append(lines, "Orca радить: "+s)
	}
	if f.Code == unknownOutcome {
		lines = append(lines, "Перевірте: Orca запущена й оновлена (CLI і застосунок мають збігатися), а репозиторій доданий в Orca. Потім натисніть «Повторити».")
	}
	return strings.Join(lines, "\n")
}

// findWorkspace: the task's workspace by branch, including Orca's numeric suffixes (feature/X-1-2). The list's
// shape varies between Orca versions, so any object with a matching branch will do.
func findWorkspace(ctx context.Context, command string, j Job) (path, branch string) {
	out, ok := run(ctx, command, j.Repo.Path, "worktree", "list", "--repo", "path:"+j.Repo.Path, "--json")
	if !ok {
		return "", ""
	}
	var root any
	if json.Unmarshal([]byte(out), &root) != nil {
		return "", ""
	}
	suffixed := regexp.MustCompile("^" + regexp.QuoteMeta(j.Branch) + `-\d+$`)
	stack := []any{root}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch n := node.(type) {
		case []any:
			stack = append(stack, n...)
		case map[string]any:
			if b, ok := n["branch"].(string); ok {
				b = strings.TrimPrefix(b, "refs/heads/")
				if b == j.Branch || suffixed.MatchString(b) {
					p, _ := n["path"].(string)
					return p, b
				}
			}
			for _, v := range n {
				stack = append(stack, v)
			}
		}
	}
	return "", ""
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
