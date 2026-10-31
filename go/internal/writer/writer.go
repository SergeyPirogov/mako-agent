// Package writer asks Claude Code (haiku, the fast one) to write merge request text from a branch's changes.
package writer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

const maxDiff = 30_000

// Claude is the command used; tests replace it.
var Claude = "claude"

// ReviewText: title and summary of an MR from everything the branch changes against its base.
func ReviewText(ctx context.Context, worktree, baseBranch string, t core.Task) (title, summary string, err error) {
	base, err := gitx.Run(ctx, worktree, "merge-base", "origin/"+baseBranch, "HEAD")
	base = strings.TrimSpace(base)
	if err != nil || base == "" {
		return "", "", fmt.Errorf("немає origin/%s", baseBranch)
	}
	commits, _ := gitx.Run(ctx, worktree, "log", "--format=- %s", base+"..HEAD")
	stat, _ := gitx.Run(ctx, worktree, "diff", "--stat", base, "HEAD")
	diff, _ := gitx.Run(ctx, worktree, "diff", base, "HEAD")
	if strings.TrimSpace(diff) == "" {
		return "", "", errors.New("немає змін")
	}
	text, err := Ask(ctx, worktree, strings.Join([]string{
		"Write the title and description of a merge request for the changes below. Base them on what the diff actually does.",
		"First line: the title, an imperative summary under 72 characters, no issue number.",
		"Then a blank line and 2-6 short bullet points (\"- \"): what changed and why, for a reviewer.",
		fmt.Sprintf("The work is for the issue %q (%s).", t.Title, t.ID),
		"Answer with the title and bullets only: no headings, no quotes, no code fences, no preface.",
		"", "Commits:", commits, stat, clip(diff),
	}, "\n"))
	if err != nil {
		return "", "", err
	}
	first, rest, _ := strings.Cut(text, "\n")
	title = strings.TrimSpace(titlePrefix.ReplaceAllString(first, ""))
	if len(title) > 120 {
		title = title[:120]
	}
	if title == "" {
		title = t.ID + ": " + t.Title
	}
	return title, strings.TrimSpace(rest), nil
}

var (
	titlePrefix = regexp.MustCompile(`(?i)^#+\s*|^title:\s*`)
	fences      = regexp.MustCompile("^```\\w*\\n?|```\\s*$")
)

// Ask runs `claude -p --model haiku` with the prompt on stdin, for up to two minutes.
func Ask(ctx context.Context, dir, prompt string) (string, error) {
	path, err := exec.LookPath(Claude)
	if err != nil {
		return "", errors.New("Claude Code не знайдено")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-p", "--model", "haiku", "--output-format", "text")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("Claude не відповів за 2 хв")
	}
	if err != nil {
		return "", fmt.Errorf("claude: %v: %s", err, clipTo(strings.TrimSpace(stderr.String()), 200))
	}
	text := strings.TrimSpace(fences.ReplaceAllString(strings.TrimSpace(string(out)), ""))
	if text == "" || len(text) > 6000 {
		return "", errors.New("Claude не дав відповіді")
	}
	return text, nil
}

func clip(diff string) string {
	if len(diff) > maxDiff {
		return fmt.Sprintf("%s\n… (diff cut at %d characters)", diff[:maxDiff], maxDiff)
	}
	return diff
}

func clipTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// TaskSummary: what the task came to, in Ukrainian, for the card (and the issue comment): from the agent's last
// words in its terminal, the commits and the diff.
func TaskSummary(ctx context.Context, worktree, baseBranch string, t core.Task, agentText, reviewURL string) (string, error) {
	var commits, stat, diff string
	if base, err := gitx.Run(ctx, worktree, "merge-base", "origin/"+baseBranch, "HEAD"); err == nil && base != "" {
		commits, _ = gitx.Run(ctx, worktree, "log", "--format=- %s", base+"..HEAD")
		stat, _ = gitx.Run(ctx, worktree, "diff", "--stat", base, "HEAD")
		diff, _ = gitx.Run(ctx, worktree, "diff", base, "HEAD")
	}
	if len(agentText) > 8000 {
		agentText = agentText[len(agentText)-8000:]
	}
	review := "No merge request was opened."
	if reviewURL != "" {
		review = "Merge request: " + reviewURL
	}
	return Ask(ctx, worktree, strings.Join([]string{
		"Summarize, in Ukrainian, what was done for this task, for the developer who will read it later on a task board.",
		"Use Markdown with these sections, each short (bullets, at most ~6 per section), skipping a section that has nothing:",
		"## Що зроблено",
		"## Як перевірено",
		"## На що звернути увагу (open questions, risks, follow-ups, anything left undone)",
		"Base it on the end of the agent's terminal session and the actual changes below. Do not invent checks that are not mentioned. Answer with the summary only.",
		"", fmt.Sprintf("Task: %s — %s", t.ID, t.Title), review,
		"", "End of the agent's terminal session:", orNone(agentText),
		"", "Commits:", orNone(commits), stat, clip(diff),
	}, "\n"))
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
