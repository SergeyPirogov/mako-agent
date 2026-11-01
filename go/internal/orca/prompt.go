package orca

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
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

// DefaultHandoffChecked: with checks in the repo's settings, the agent commits and Mako runs them and opens the
// MR itself once they pass.
const DefaultHandoffChecked = "## Finish with commits: Mako checks them and opens the merge request\n" +
	"1. Implement the task and check it (tests, linters) as described above.\n" +
	"2. Commit on the branch {branch}. Each message: a first line under 72 characters that says what the change does,\n" +
	"   in the imperative (\"Fix pagination size in the stubs admin\"), starting with \"{taskId}: \"; then a blank line and\n" +
	"   1-5 bullets on what changed and why. No \"fix\", \"wip\" or \"update\" on their own: Mako writes the merge request\n" +
	"   from these messages.\n" +
	"3. Do not push and do not open a merge request. When your turn ends with everything committed, Mako runs the\n" +
	"   project's checks in this worktree:\n" +
	"{checks}\n" +
	"   If they pass, Mako pushes the branch and opens the merge request into {baseBranch}. If one fails, you get the\n" +
	"   last lines of its output here: fix the cause, commit, and end your turn again.\n" +
	"4. Reply with a short summary: what changed and how you checked it."

// checksNote: what a custom end of the prompt gets when Mako checks the branch and opens the MR.
const checksNote = "## Mako opens the merge request\n" +
	"Do not push and do not open a merge request yourself, whatever is said above: commit on the branch {branch}.\n" +
	"When your turn ends with everything committed, Mako runs the project's checks in this worktree:\n" +
	"{checks}\n" +
	"If they pass, Mako pushes the branch and opens the merge request into {baseBranch}. If one fails, you get the\n" +
	"last lines of its output here: fix the cause, commit, and end your turn again."

// FullPrompt: the task, what to do differently when it is a new attempt, screenshots for UI changes, the
// language to write in and how to finish.
func FullPrompt(j Job, o config.Orca) string {
	parts := []string{Prompt(j.Task, j.TrackerName)}
	if again := Again(j.RetryNote, j.LastAttempt); again != "" {
		parts = append(parts, again)
	}
	if shots := Screenshots(j); shots != "" {
		parts = append(parts, shots)
	}
	parts = append(parts, i18n.AgentNote(i18n.Lang()))
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

// MakoCommand: how the agent runs this Mako (its executable, quoted for the shell); "" leaves screenshots out of
// the prompt.
var MakoCommand = ""

// Screenshots: how the agent shows a UI change, «before» and «after», in the chat and the MR.
func Screenshots(j Job) string {
	if MakoCommand == "" {
		return ""
	}
	base := "HEAD" // origin/HEAD: the remote's default branch
	if j.Repo != nil && j.Repo.BaseBranch != "" {
		base = j.Repo.BaseBranch
	}
	shot := MakoCommand + " screenshot -task " + ShellQuote(j.Task.ID)
	return "## Screenshots for UI changes\n" +
		"If the task changes what people see (a page, a form, the board, the settings), show it «before» and «after»:\n" +
		"1. Before you change it: `" + shot + " -name before -rev origin/" + base + "`; once it works: `" + shot + " -name after`.\n" +
		"   Add `-page /settings` (or the page you changed; `-size 390x844` for a phone) when it is not the main page.\n" +
		"   This builds Mako from the worktree (or the revision) and runs it read-only on a free port with a copy of the\n" +
		"   current Mako's data, then photographs it in headless Chrome. For a project other than Mako, run its local\n" +
		"   build on a free port (the base branch in a temporary `git worktree` for «before») and add `-url http://127.0.0.1:<port>/<page>`.\n" +
		"2. Mako shows each screenshot in the task's chat on its board; the command prints a Markdown image for it.\n" +
		"   Put both in the merge request description under \"## Screenshots\" (before, then after).\n" +
		"3. If a screenshot cannot be taken (no Chrome, the app does not start), say why in the description."
}

// ShellQuote: s as one word for the shell.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
	checked := len(j.Checks) > 0 && mrBy != "me"
	if checked {
		lines := make([]string, len(j.Checks))
		for i, c := range j.Checks {
			lines[i] = "   - `" + c + "`"
		}
		values["checks"] = strings.Join(lines, "\n")
	}
	text := strings.TrimSpace(custom)
	switch {
	case text != "" && checked:
		text += "\n\n" + checksNote
	case text != "":
	case checked:
		text = DefaultHandoffChecked
	case mrBy == "me":
		text = DefaultHandoffForMe
	default:
		text = DefaultHandoff
	}
	return placeholder.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := values[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}
