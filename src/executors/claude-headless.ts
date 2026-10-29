import { spawn } from "node:child_process";
import { createWriteStream } from "node:fs";
import * as git from "../git.js";
import { log } from "../log.js";
import { track } from "./processes.js";
import type { ExecContext, ExecOutcome, Executor } from "./types.js";

export interface ClaudeHeadlessConfig {
  type: "claude-headless";
  label?: string;
  command?: string;
  args?: string[];
}

/** Runs `claude -p` in a Mako-made worktree and waits, streaming JSON events into the log. */
export class ClaudeHeadlessExecutor implements Executor {
  readonly label: string;
  readonly interactive = false;
  readonly managed = true;

  constructor(readonly name: string, private cfg: ClaudeHeadlessConfig) {
    this.label = cfg.label ?? "Claude Code";
  }

  async run(ctx: ExecContext, prompt: string): Promise<ExecOutcome> {
    const worktree = await git.prepareWorktree(ctx.repo, ctx.task.slug, (line) => log(ctx.task.id, line));
    const args = ["-p", prompt, "--output-format", "stream-json", "--verbose", ...(this.cfg.args ?? [])];
    const child = spawn(this.cfg.command ?? "claude", args, { cwd: worktree, stdio: ["ignore", "pipe", "pipe"] });
    track(child);
    const logFile = createWriteStream(ctx.logPath, { flags: "a" });

    return new Promise((resolve) => {
      let result: ExecOutcome | undefined;
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
                kind: "completed",
                ok: !event.is_error && event.subtype === "success",
                summary: event.result ?? `Agent stopped: ${event.subtype}`,
                worktree,
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

      const timer = setTimeout(() => child.kill("SIGTERM"), ctx.timeoutMin * 60_000);
      const finish = (summary: string) => {
        clearTimeout(timer);
        logFile.end();
        resolve(result ?? { kind: "completed", ok: false, summary, worktree });
      };
      child.on("error", (err) => finish(`Failed to start ${this.cfg.command ?? "claude"}: ${err.message}`));
      child.on("close", (code, signal) =>
        finish(
          signal
            ? `Agent was terminated (${signal}), possibly by the ${ctx.timeoutMin} min timeout`
            : `Agent exited with code ${code}: ${stderr.slice(-2000)}`,
        ),
      );
    });
  }
}
