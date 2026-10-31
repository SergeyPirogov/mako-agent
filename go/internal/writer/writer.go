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
	commits, stat, diff := BranchWork(ctx, worktree, baseBranch)
	agentText = lastOf(agentText, 8000)
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

// BranchWork: the branch's commits, diffstat and diff against its base; empty when there is no base.
func BranchWork(ctx context.Context, worktree, baseBranch string) (commits, stat, diff string) {
	if base, err := gitx.Run(ctx, worktree, "merge-base", "origin/"+baseBranch, "HEAD"); err == nil && base != "" {
		commits, _ = gitx.Run(ctx, worktree, "log", "--format=- %s", base+"..HEAD")
		stat, _ = gitx.Run(ctx, worktree, "diff", "--stat", base, "HEAD")
		diff, _ = gitx.Run(ctx, worktree, "diff", base, "HEAD")
	}
	return commits, stat, diff
}

func lastOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// AttemptSummary: a short account of an attempt the developer is dropping, for the next attempt's prompt: the
// approach it took, what it changed, and what went wrong, read against the developer's note.
func AttemptSummary(ctx context.Context, worktree, baseBranch string, t core.Task, agentText, note string) (string, error) {
	commits, stat, diff := BranchWork(ctx, worktree, baseBranch)
	return Ask(ctx, worktree, strings.Join([]string{
		"A coding agent worked on this task, and the developer is dropping its attempt to start over on a fresh branch.",
		"Write, in English, a short account for the agent of the next attempt: at most 6 bullets, no headings.",
		"Cover the approach the attempt took, what it changed (files, key decisions), and what did not work or was left undone;",
		"use the developer's note to tell what went wrong. Do not invent anything not in the material below. Answer with the bullets only.",
		"", fmt.Sprintf("Task: %s — %s", t.ID, t.Title),
		"", "The developer's note on what to do differently:", orNone(note),
		"", "End of the agent's session:", orNone(lastOf(agentText, 8000)),
		"", "Commits:", orNone(commits), stat, clip(diff),
	}, "\n"))
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// Lesson: the rule for the repo's next agents in what you told this one, in one short line; "" when the
// message is no such correction (a request, a question, a «carry on») or CLAUDE.md already says it.
func Lesson(ctx context.Context, dir, message, claudeMD string) (string, error) {
	if len(message) > 8000 {
		message = message[:8000]
	}
	if len(claudeMD) > 8000 {
		claudeMD = claudeMD[:8000]
	}
	text, err := Ask(ctx, dir, strings.Join([]string{
		"A developer wrote the message below to a coding agent working in this repository, in a chat or as review comments.",
		"Decide whether it corrects how the agent works in a way that should hold for every future task in this repository",
		"(a convention, a place not to touch, how tests are written or run here, a tool to use or avoid).",
		"A one-off request about this task only, a question, a «carry on» or praise is not such a correction.",
		"If it is one and CLAUDE.md below does not say it already, answer with the rule only: one imperative sentence,",
		"under 200 characters, in the language of the message, specific enough to follow without this conversation.",
		"Otherwise answer exactly NONE.",
		"", "Message:", message,
		"", "CLAUDE.md now:", orNone(claudeMD),
	}, "\n"))
	if err != nil {
		return "", err
	}
	rule := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(firstNonEmpty(text)), "-*• "))
	rule = strings.Trim(rule, "\"«»")
	if rule == "" || strings.EqualFold(strings.Trim(rule, ". "), "none") || len(rule) > 400 {
		return "", nil
	}
	return rule, nil
}

func firstNonEmpty(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}
