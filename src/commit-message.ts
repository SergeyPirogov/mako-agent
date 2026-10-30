import { spawn } from "node:child_process";
import { findCommand } from "./executors/find-command.js";
import { run } from "./shell.js";
import type { Task } from "./types.js";

/**
 * A commit message for the worktree's staged changes, written by Claude from the diff; the task id and title
 * when Claude is not installed or does not answer in time.
 */
export async function commitMessage(worktree: string, task: Task): Promise<string> {
  const fallback = `${task.id}: ${task.title}`;
  const claude = findCommand("claude");
  if (!claude) return fallback;
  const stat = (await run("git", ["diff", "--cached", "--stat"], worktree)).stdout;
  const diff = (await run("git", ["diff", "--cached"], worktree)).stdout.slice(0, 20_000);
  if (!diff.trim()) return fallback;
  const prompt = [
    "Write a git commit message for the staged changes below.",
    "First line: an imperative summary under 72 characters. Then a blank line and 1-4 short bullet points on what changed and why.",
    `The work is for the issue "${task.title}" (${task.id}); end with the line "Refs ${task.id}".`,
    "Answer with the commit message only: no quotes, no code fences, no preface.",
    "",
    stat,
    diff,
  ].join("\n");
  const text = await ask(claude, prompt, worktree, 90_000).catch(() => "");
  const message = text.replace(/^```\w*\n?|```$/g, "").trim();
  return message && message.length < 4000 ? message : fallback;
}

function ask(claude: string, prompt: string, cwd: string, timeoutMs: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn(claude, ["-p", "--model", "haiku", "--output-format", "text"], { cwd, stdio: ["pipe", "pipe", "ignore"] });
    let out = "";
    const timer = setTimeout(() => child.kill("SIGTERM"), timeoutMs);
    child.stdout.on("data", (d) => (out += d));
    child.on("error", reject);
    child.on("close", (code) => {
      clearTimeout(timer);
      code === 0 ? resolve(out) : reject(new Error(`claude exited with ${code}`));
    });
    child.stdin.end(prompt);
  });
}
