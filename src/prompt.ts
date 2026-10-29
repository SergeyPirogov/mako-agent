import type { RepoConfig } from "./config.js";
import type { Task, Tracker } from "./types.js";

/**
 * `interactive`: a developer watches the session and may answer questions.
 * `managed`: Mako commits, pushes and opens the PR afterwards, so the agent must not.
 */
export function buildPrompt(
  task: Task,
  tracker: Tracker,
  repo: RepoConfig,
  mode: { interactive: boolean; managed: boolean } = { interactive: false, managed: true },
): string {
  const { interactive, managed } = mode;
  const comments = task.comments.length
    ? task.comments.map((c) => `**${c.author}:**\n${c.body.trim()}`).join("\n\n")
    : "(none)";
  const intro = interactive
    ? "A developer is watching this session; ask them if something important is unclear."
    : "Nobody will answer questions, so make reasonable decisions yourself.";
  return `You are working on ${tracker.name} issue ${task.id}. ${intro}

# ${task.id}: ${task.title}
${task.meta ? task.meta + " | " : ""}${task.url}

## Description
${task.description.trim() || "(empty)"}

## Comments
${comments}

## How to work
- You are in a dedicated git worktree for this task. Stay inside it.
- Explore the codebase first, then implement the change with minimal, focused edits.
- Run the relevant tests/linters if the project has them.
- Commit your work with messages that start with "${task.id}: ".
${managed ? "- Do not push and do not open pull requests; Mako does that when the session ends.\n" : ""}- If the task is unclear or impossible, make no changes and explain why.
${repo.extraInstructions ? `- ${repo.extraInstructions}\n` : ""}
Finish with a short summary for the issue comment: what you changed, how you verified it, and anything a reviewer should check.`;
}
