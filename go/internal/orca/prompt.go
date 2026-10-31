package orca

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Prompt: the task as the agent in Orca sees it. A developer follows the session in Orca.
func Prompt(t core.Task, trackerName string) string {
	comments := "(none)"
	if len(t.Comments) > 0 {
		var parts []string
		for _, c := range t.Comments {
			parts = append(parts, fmt.Sprintf("**%s:**\n%s", c.Author, strings.TrimSpace(c.Body)))
		}
		comments = strings.Join(parts, "\n\n")
	}
	meta := t.URL
	if t.Meta != "" {
		meta = t.Meta + " | " + t.URL
	}
	description := strings.TrimSpace(t.Description)
	if description == "" {
		description = "(empty)"
	}
	return fmt.Sprintf(`You are working on %s issue %s. A developer is watching this session; ask them if something important is unclear.

# %s: %s
%s

## Description
%s

## Comments
%s

## How to work
- You are in a dedicated git worktree for this task. Stay inside it.
- Explore the codebase first, then implement the change with minimal, focused edits.
- Run the relevant tests/linters if the project has them.
- Never bypass git hooks (no --no-verify). If a hook or check fails, fix the cause; if you cannot, say so.
- If the task is unclear or impossible, make no changes and explain why.`, trackerName, t.ID, t.ID, t.Title, meta, description, comments)
}

// DefaultHandoff: the agent finishes with a pushed branch and the MR Mako watches for.
const DefaultHandoff = "## Finish with a merge request\n" +
	"Take the task all the way to a merge request yourself:\n" +
	"1. Implement it and check it (tests, linters) as described above.\n" +
	"2. Commit on the branch {branch}. Each message: a first line under 72 characters that says what the change does,\n" +
	"   in the imperative (\"Fix pagination size in the stubs admin\"), starting with \"{taskId}: \"; then a blank line and\n" +
	"   1-5 bullets on what changed and why. No \"fix\", \"wip\" or \"update\" on their own.\n" +
	"3. Push the branch: git push -u origin {branch}\n" +
	"4. Open a merge request from {branch} into {baseBranch}. Title: what it does, in a sentence, not the branch name.\n" +
	"   Description in Markdown, short, from the actual diff:\n" +
	"   ## What changed\n" +
	"   - 2-6 bullets: what and why\n" +
	"   ## How it was checked\n" +
	"   - the tests and checks you ran, with results\n" +
	"   ## Worth a look\n" +
	"   - risks, open questions, anything left undone (skip if none)\n" +
	"   {closingRef}\n" +
	"   GitLab: `glab mr create --target-branch {baseBranch} --fill` when glab is installed, otherwise\n" +
	"   `git push -o merge_request.create -o merge_request.target={baseBranch} -o merge_request.title=\"…\" origin {branch}`.\n" +
	"   GitHub: `gh pr create --base {baseBranch} --fill`.\n" +
	"5. Reply with the link to the merge request.\n" +
	"Mako watches the branch: once the merge request exists, it moves the issue on and comments with the link."

// DefaultHandoffForMe: the developer opens the MR in Orca after a look; the agent stops at commits.
const DefaultHandoffForMe = "## Finish with commits, not a merge request\n" +
	"1. Implement the task and check it (tests, linters) as described above.\n" +
	"2. Commit on the branch {branch}. Each message: a first line under 72 characters that says what the change does,\n" +
	"   in the imperative (\"Fix pagination size in the stubs admin\"), starting with \"{taskId}: \"; then a blank line and\n" +
	"   1-5 bullets on what changed and why. No \"fix\", \"wip\" or \"update\" on their own: Orca writes the merge request\n" +
	"   from these messages.\n" +
	"3. Do not push and do not open a merge request: the developer reviews the changes and opens it from Orca.\n" +
	"4. Reply with a ready merge request: a title (what it does, in a sentence) and a Markdown description with\n" +
	"   \"## What changed\", \"## How it was checked\", \"## Worth a look\", ending with \"{closingRef}\"."

// FullPrompt: the task, what to do differently when it is a new attempt, and how to finish.
func FullPrompt(j Job, o config.Orca) string {
	parts := []string{Prompt(j.Task, j.TrackerName)}
	if again := Again(j.RetryNote, j.LastAttempt); again != "" {
		parts = append(parts, again)
	}
	return strings.Join(append(parts, Instructions(o.Instructions, o.MRBy, j)), "\n\n")
}

// Again: the part of a new attempt's prompt about the one before it; "" for a first attempt.
func Again(note, last string) string {
	note, last = strings.TrimSpace(note), strings.TrimSpace(last)
	if note == "" && last == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## This is a new attempt\n")
	b.WriteString("An earlier attempt at this task did not work out; its branch and merge request were dropped, and you start from the base branch.")
	if last != "" {
		b.WriteString("\n\n### What the last attempt did\n" + last)
	}
	if note != "" {
		b.WriteString("\n\n### What the developer wants done differently\n" + note + "\n\nFollow this over anything the last attempt did.")
	}
	return b.String()
}

var placeholder = regexp.MustCompile(`\{(\w+)\}`)

// Instructions: the end of the prompt, custom or the default for who opens the MR, with placeholders filled.
func Instructions(custom, mrBy string, j Job) string {
	closing := j.Task.ClosingRef
	if closing == "" {
		closing = j.Task.URL
	}
	values := map[string]string{
		"branch": j.Branch, "baseBranch": j.Repo.BaseBranch, "taskId": j.Task.ID,
		"title": j.Task.Title, "url": j.Task.URL, "closingRef": closing,
	}
	text := strings.TrimSpace(custom)
	if text == "" {
		text = DefaultHandoff
		if mrBy == "me" {
			text = DefaultHandoffForMe
		}
	}
	return placeholder.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := values[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}
