import { spawn, type ChildProcess } from "node:child_process";
import { createWriteStream } from "node:fs";
import type { Config, ProjectConfig } from "./config.js";
import type { Task, Tracker } from "./types.js";

export interface AgentResult {
  ok: boolean;
  summary: string;
  costUsd?: number;
  sessionId?: string;
}

const running = new Set<ChildProcess>();

export function killAllAgents(): void {
  for (const child of running) child.kill("SIGTERM");
}

export function buildPrompt(task: Task, tracker: Tracker, project: ProjectConfig): string {
  const comments = task.comments.length
    ? task.comments.map((c) => `**${c.author}:**\n${c.body.trim()}`).join("\n\n")
    : "(none)";
  return `You are working autonomously on ${tracker.name} issue ${task.id}. Nobody will answer questions, so make reasonable decisions yourself.

# ${task.id}: ${task.title}
${task.meta ? task.meta + " | " : ""}${task.url}

## Description
${task.description.trim() || "(empty)"}

## Comments
${comments}

## How to work
- You are in a dedicated git worktree on branch agent/${task.slug}. Stay inside it.
- Explore the codebase first, then implement the change with minimal, focused edits.
- Run the relevant tests/linters if the project has them.
- Commit your work with messages that start with "${task.id}: ".
- Do not push and do not open pull requests; the orchestrator does that.
- If the task is unclear or impossible, make no changes and explain why.
${project.extraInstructions ? `- ${project.extraInstructions}\n` : ""}
Finish with a short summary for the issue comment: what you changed, how you verified it, and anything a reviewer should check.`;
}

/** Runs headless Claude Code in the worktree and streams its JSON events into a log file. */
export function runAgent(
  cfg: Config,
  prompt: string,
  cwd: string,
  logPath: string,
): Promise<AgentResult> {
  return new Promise((resolve) => {
    const args = ["-p", prompt, "--output-format", "stream-json", "--verbose", ...cfg.claude.args];
    const child = spawn(cfg.claude.command, args, { cwd, stdio: ["ignore", "pipe", "pipe"] });
    running.add(child);
    const logFile = createWriteStream(logPath, { flags: "a" });

    let result: AgentResult | undefined;
    let buffer = "";
    let stderr = "";

    child.stdout.on("data", (chunk: Buffer) => {
      logFile.write(chunk);
      buffer += chunk.toString();
      let nl;
      while ((nl = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, nl).trim();
        buffer = buffer.slice(nl + 1);
        if (!line) continue;
        try {
          const event = JSON.parse(line);
          if (event.type === "result") {
            result = {
              ok: !event.is_error && event.subtype === "success",
              summary: event.result ?? `Agent stopped: ${event.subtype}`,
              costUsd: event.total_cost_usd,
              sessionId: event.session_id,
            };
          }
        } catch {
          // non-JSON line; already in the log
        }
      }
    });
    child.stderr.on("data", (d) => {
      stderr += d;
      logFile.write(d);
    });

    const timer = setTimeout(() => child.kill("SIGTERM"), cfg.taskTimeoutMin * 60_000);

    const finish = (fallback: AgentResult) => {
      clearTimeout(timer);
      running.delete(child);
      logFile.end();
      resolve(result ?? fallback);
    };
    child.on("error", (err) => finish({ ok: false, summary: `Failed to start agent: ${err.message}` }));
    child.on("close", (code, signal) =>
      finish({
        ok: false,
        summary: signal
          ? `Agent was terminated (${signal}), possibly by the ${cfg.taskTimeoutMin} min timeout`
          : `Agent exited with code ${code}: ${stderr.slice(-2000)}`,
      }),
    );
  });
}
