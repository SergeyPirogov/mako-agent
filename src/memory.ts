import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { write } from "./commit-message.js";
import { run } from "./shell.js";
import type { Task } from "./types.js";

/**
 * What agents learned about a repository, kept by Mako between tasks: how to run the tests, where things are,
 * conventions and pitfalls. Plain Markdown in the state folder, one file per repo, editable on its page.
 */
const MAX = 8000;

const fileOf = (stateDir: string, repo: string) => path.join(stateDir, "memory", `${repo.replace(/[^\w.-]+/g, "_")}.md`);

export function readMemory(stateDir: string, repo: string): string {
  const file = fileOf(stateDir, repo);
  return existsSync(file) ? readFileSync(file, "utf8").trim().slice(0, MAX) : "";
}

export function writeMemory(stateDir: string, repo: string, text: string): void {
  const file = fileOf(stateDir, repo);
  if (!text.trim()) return void rmSync(file, { force: true });
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, text.trim().slice(0, MAX) + "\n");
}

/** After a finished task: Claude folds what the agent found out into the repo's notes. Returns whether they changed. */
export async function learn(
  stateDir: string,
  repo: string,
  worktree: string,
  baseBranch: string,
  task: Task,
  agentText: string,
): Promise<boolean> {
  const current = readMemory(stateDir, repo);
  const base = (await run("git", ["merge-base", `origin/${baseBranch}`, "HEAD"], worktree).catch(() => undefined))?.stdout.trim();
  const stat = base ? (await run("git", ["diff", "--stat", base, "HEAD"], worktree).catch(() => undefined))?.stdout ?? "" : "";
  const prompt = [
    `You keep the notes about the repository «${repo}» that every new AI agent working here gets first.`,
    "Update them with what the agent below found out. Keep only durable facts that save the next agent time:",
    "how to install, run tests and linters (exact commands), project layout, conventions, pitfalls, commands that needed permission.",
    "Do not keep anything about this particular task. Drop notes the new facts contradict. Keep it short: at most ~30 bullets",
    "under a few short headings, in English. If nothing new was learned, return the notes unchanged.",
    "Answer with the full updated notes in Markdown only, nothing else.",
    "",
    "## Current notes",
    current || "(none yet)",
    "",
    `## What the agent did for ${task.id}: ${task.title}`,
    agentText.slice(0, 6000) || "(no final message)",
    "",
    "## Files it changed",
    stat || "(none)",
  ].join("\n");
  const updated = await write(worktree, prompt, "");
  if (!updated.text || updated.error || updated.text.trim() === current) return false;
  writeMemory(stateDir, repo, updated.text);
  return true;
}
