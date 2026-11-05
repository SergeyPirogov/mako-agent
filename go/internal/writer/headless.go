package writer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/proc"
)

// HeadlessOpts: how Headless runs the agent.
type HeadlessOpts struct {
	Route core.Route
	// Plan: plan mode, the agent reads and writes its plan and changes nothing.
	Plan bool
	// Budget: the most the call may spend, USD (claude's --max-budget-usd); 0: no limit of its own.
	Budget  float64
	Timeout time.Duration
}

// HeadlessTools: what the agent with no person may use besides reading: edit files and run commands.
var HeadlessTools = []string{"Bash", "Edit", "Write", "NotebookEdit"}

// HeadlessDenied: what it may not: ask a question nobody would answer, push or reach the code host. The worktree's
// pre-push hook stops a push hidden from these rules too.
var HeadlessDenied = []string{"AskUserQuestion", "Bash(git push:*)", "Bash(gh:*)", "Bash(glab:*)"}

// Headless runs a coding agent with no person in dir (an eval run): `claude -p`, keeping no session, edits accepted
// and the tools of HeadlessTools allowed, those of HeadlessDenied not. Its answer is its last message (the plan, in
// plan mode); its spend goes to the context's WithSpend, also when it fails.
func Headless(ctx context.Context, dir, prompt string, o HeadlessOpts) (string, error) {
	path, err := exec.LookPath(Claude)
	if err != nil {
		return "", errors.New("no Claude Code found")
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := proc.Command(ctx, path, headlessArgs(o)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(cmd.Env, "MAKO_EVAL=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := run(cmd)
	text := ""
	if out != nil {
		text = answer(ctx, out, o.Route.Model)
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return "", fmt.Errorf("the agent did not finish within %d min", int(o.Timeout.Minutes()))
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", fmt.Errorf("claude: %v: %s", err, failDetail(stderr.String(), out))
	}
	return strings.TrimSpace(text), nil
}

// headlessArgs: the arguments of Headless's `claude -p`.
func headlessArgs(o HeadlessOpts) []string {
	mode := "acceptEdits"
	if o.Plan {
		mode = "plan"
	}
	args := append([]string{"-p"}, streamArgs...)
	args = append(args, "--permission-mode", mode)
	if o.Budget > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(o.Budget, 'f', 2, 64))
	}
	args = append(args, modelArgs(o.Route)...)
	// Last: each takes several values.
	args = append(args, "--disallowedTools")
	args = append(args, HeadlessDenied...)
	if o.Plan {
		args = append(args, "ExitPlanMode") // nobody would approve it: the plan is its last message
	} else {
		args = append(args, "--allowedTools")
		args = append(args, HeadlessTools...)
	}
	return args
}
