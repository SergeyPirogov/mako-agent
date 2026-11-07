package writer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/proc"
)

// IntakeTimeout: how long the intake of a task may take.
const IntakeTimeout = 5 * time.Minute

// intakeMax: the longest answer the intake takes: an answer to a question about the code, with its places.
const intakeMax = 12000

// IntakeTools: what the intake may use: it reads the code and its history, it changes nothing.
var IntakeTools = append(slices.Clone(ReviewerTools), "Bash(git grep:*)")

// Intake: the agent looks at a task as it is taken, before any branch: in the repository's clone (dir), with tools
// that only read, it reads the task, looks at the code (for a bug, follows the code to reproduce it) and decides
// whether to work on it, ask in the tracker first, answer it there, or split it (only when canSplit: the tracker
// can open the subtasks). base: the branch the newest code is on, origin/<base>. Its prompt is in Mako's language.
func Intake(ctx context.Context, dir, base string, route core.Route, t core.Task, canSplit bool) (core.Intake, error) {
	split := ""
	if canSplit {
		split = i18n.TA("agent.intakeSplit")
	}
	prompt := i18n.TA("agent.intake", base, split) + "\n\n" + taskText(t, 8000, true)
	return askParsed(func() (string, error) { return runIntake(ctx, dir, prompt, route) }, func(s string) (core.Intake, error) {
		in, err := ParseIntake(s)
		if err == nil && in.Decision == core.IntakeSplit && !canSplit {
			return core.Intake{}, errors.New("the intake proposed a split the tracker cannot open")
		}
		return in, err
	})
}

// runIntake runs the intake in dir: read-only tools, no session, up to IntakeTimeout; its answer, its spend told to
// the context's WithSpend.
func runIntake(ctx context.Context, dir, prompt string, route core.Route) (string, error) {
	return runReadOnly(ctx, dir, prompt, route, IntakeTimeout, intakeMax, nil)
}

// runReadOnly runs `claude -p` in dir with tools that only read (IntakeTools), no session, up to timeout; addDirs:
// more folders it may read (the pictures of an idea). Its answer of up to most bytes, its spend told to the
// context's WithSpend.
func runReadOnly(ctx context.Context, dir, prompt string, route core.Route, timeout time.Duration, most int, addDirs []string) (string, error) {
	path, err := exec.LookPath(Claude)
	if err != nil {
		return "", errors.New("no Claude Code found")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string{"-p"}, streamArgs...)
	args = append(args, "--permission-mode", "default")
	if len(addDirs) > 0 {
		args = append(append(args, "--add-dir"), addDirs...)
	}
	args = append(args, "--disallowedTools", "Edit", "Write", "MultiEdit", "NotebookEdit", "--allowedTools")
	args = append(args, IntakeTools...)
	// After the tools: --allowedTools takes several values.
	args = append(args, modelArgs(route)...)
	cmd := proc.Command(ctx, path, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := run(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("no answer from Claude within %d min", int(timeout.Minutes()))
	}
	if err != nil {
		return "", fmt.Errorf("claude: %v: %s", err, failDetail(stderr.String(), out))
	}
	text := strings.TrimSpace(answer(ctx, out, route.Model))
	if text == "" || len(text) > most {
		return "", errors.New("empty answer from Claude")
	}
	return text, nil
}

// ParseIntake reads the JSON the intake asks for. An unknown decision, a clarify without questions or an answer
// without its text is an error: the task then goes to work as without the intake.
func ParseIntake(answer string) (core.Intake, error) {
	raw, err := jsonObject(answer, "decision")
	if err != nil {
		return core.Intake{}, err
	}
	var in struct {
		Decision, Why, Answer string
		Questions             []string
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return core.Intake{}, fmt.Errorf("%w: %v", errNotJSON, err)
	}
	i := core.Intake{State: "done", Decision: strings.ToLower(strings.TrimSpace(in.Decision)), Why: clipTo(strings.TrimSpace(in.Why), 500)}
	if !slices.Contains(core.IntakeDecisions, i.Decision) {
		return core.Intake{}, fmt.Errorf("unknown decision %q", in.Decision)
	}
	switch i.Decision {
	case core.IntakeClarify:
		for _, q := range in.Questions {
			if q = strings.TrimSpace(q); q != "" && len(i.Questions) < 5 {
				i.Questions = append(i.Questions, clipTo(q, 500))
			}
		}
		if len(i.Questions) == 0 {
			return core.Intake{}, errors.New("the intake would ask in the tracker, but has no questions")
		}
	case core.IntakeAnswer:
		i.Answer = strings.TrimSpace(in.Answer)
		if i.Answer == "" {
			return core.Intake{}, errors.New("the intake would answer in the tracker, but has no answer")
		}
	}
	return i, nil
}
