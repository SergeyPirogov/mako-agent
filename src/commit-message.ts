import { spawn } from "node:child_process";
import { findCommand } from "./executors/find-command.js";
import { run } from "./shell.js";
import type { Task } from "./types.js";

export interface Written {
  text: string;
  /** Why the fallback was used, when Claude did not write it. */
  error?: string;
}

const MAX_DIFF = 30_000;

/** A commit message for the staged changes, written by Claude from their diff. */
export async function commitMessage(worktree: string, task: Task): Promise<Written> {
  const fallback = `${task.id}: ${task.title}`;
  const stat = (await run("git", ["diff", "--cached", "--stat"], worktree)).stdout;
  const diff = (await run("git", ["diff", "--cached"], worktree)).stdout;
  if (!diff.trim()) return { text: fallback, error: "немає змін" };
  return write(
    worktree,
    [
      "Write a git commit message for the staged changes below. Describe what the code changes do, based on the diff.",
      "First line: an imperative summary under 72 characters. Then a blank line and 1-5 short bullet points: what changed and why.",
      `The work is for the issue "${task.title}" (${task.id}); end with the line "Refs ${task.id}".`,
      "Answer with the commit message only: no quotes, no code fences, no preface.",
      "",
      stat,
      clip(diff),
    ].join("\n"),
    fallback,
  );
}

/**
 * Title and summary of a merge request from everything the branch changes against its base:
 * the agent's own commits included, not only what was left uncommitted.
 */
export async function reviewText(worktree: string, baseBranch: string, task: Task): Promise<{ title: string; summary: string; error?: string }> {
  const fallback = { title: `${task.id}: ${task.title}`, summary: "" };
  const base = (await run("git", ["merge-base", `origin/${baseBranch}`, "HEAD"], worktree)).stdout.trim();
  if (!base) return { ...fallback, error: `немає origin/${baseBranch}` };
  const commits = (await run("git", ["log", "--format=- %s", `${base}..HEAD`], worktree)).stdout;
  const stat = (await run("git", ["diff", "--stat", base, "HEAD"], worktree)).stdout;
  const diff = (await run("git", ["diff", base, "HEAD"], worktree)).stdout;
  if (!diff.trim()) return { ...fallback, error: "немає змін" };
  const written = await write(
    worktree,
    [
      "Write the title and description of a merge request for the changes below. Base them on what the diff actually does.",
      "First line: the title, an imperative summary under 72 characters, no issue number.",
      "Then a blank line and 2-6 short bullet points (\"- \"): what changed and why, for a reviewer.",
      `The work is for the issue "${task.title}" (${task.id}).`,
      "Answer with the title and bullets only: no headings, no quotes, no code fences, no preface.",
      "",
      "Commits:",
      commits,
      stat,
      clip(diff),
    ].join("\n"),
    "",
  );
  if (!written.text) return { ...fallback, error: written.error };
  const [title, ...rest] = written.text.split("\n");
  return { title: title.replace(/^#+\s*|^title:\s*/i, "").trim().slice(0, 120) || fallback.title, summary: rest.join("\n").trim() };
}

async function write(cwd: string, prompt: string, fallback: string): Promise<Written> {
  const claude = findCommand("claude");
  if (!claude) return { text: fallback, error: "Claude Code не знайдено" };
  try {
    const text = (await ask(claude, prompt, cwd, 120_000)).replace(/^```\w*\n?|```\s*$/g, "").trim();
    if (!text || text.length > 6000) return { text: fallback, error: "Claude не дав відповіді" };
    return { text };
  } catch (err: any) {
    return { text: fallback, error: err.message };
  }
}

function clip(diff: string): string {
  return diff.length > MAX_DIFF ? `${diff.slice(0, MAX_DIFF)}\n… (diff cut at ${MAX_DIFF} characters)` : diff;
}

function ask(claude: string, prompt: string, cwd: string, timeoutMs: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn(claude, ["-p", "--model", "haiku", "--output-format", "text"], { cwd, stdio: ["pipe", "pipe", "pipe"] });
    let out = "";
    let err = "";
    const timer = setTimeout(() => child.kill("SIGTERM"), timeoutMs);
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (err += d));
    child.on("error", reject);
    child.on("close", (code, signal) => {
      clearTimeout(timer);
      if (code === 0) return resolve(out);
      reject(new Error(signal ? "Claude не відповів за 2 хв" : `claude завершився з кодом ${code}: ${err.trim().slice(0, 200)}`));
    });
    child.stdin.end(prompt);
  });
}
