import { spawn } from "node:child_process";
import { findCommand } from "./executors/find-command.js";

/**
 * A plan before any code: Claude Code in plan mode reads the repository (it cannot change anything there)
 * and answers with the steps it would take, for the developer to approve or correct.
 */
export async function makePlan(worktree: string, prompt: string, timeoutMs = 10 * 60_000): Promise<string> {
  const claude = findCommand("claude");
  if (!claude) throw new Error("Для плану потрібен Claude Code: команду claude не знайдено");
  const ask = [
    prompt,
    "",
    "---",
    "Before any change, the developer wants to approve a plan. Do not modify any files now.",
    "Investigate the code as much as you need, then answer with the plan only, in Markdown:",
    "## Як я це зроблю — numbered steps, naming the files you will change and why;",
    "## Як перевірю — the tests or commands you will run;",
    "## Питання й ризики — anything unclear or risky (skip if none).",
    "Write the plan in Ukrainian; keep code, file names and commands as they are.",
  ].join("\n");
  return new Promise((resolve, reject) => {
    const child = spawn(claude, ["-p", "--permission-mode", "plan", "--output-format", "text"], { cwd: worktree, stdio: ["pipe", "pipe", "pipe"] });
    let out = "";
    let err = "";
    const timer = setTimeout(() => child.kill("SIGTERM"), timeoutMs);
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (err += d));
    child.on("error", reject);
    child.on("close", (code, signal) => {
      clearTimeout(timer);
      if (code === 0 && out.trim()) return resolve(out.trim());
      reject(new Error(signal ? "Claude не склав план за 10 хв" : `claude завершився з кодом ${code}: ${err.trim().slice(0, 300)}`));
    });
    // Claude may exit before reading it all (no login, a bad flag): its exit code reports that, not a crash.
    child.stdin.on("error", () => {});
    child.stdin.end(ask);
  });
}
