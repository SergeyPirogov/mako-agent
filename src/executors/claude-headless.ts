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
    this.label = cfg.label ?? "Claude Code у фоні";
  }

  async run(ctx: ExecContext, prompt: string): Promise<ExecOutcome> {
    const worktree = await git.prepareWorktree(ctx.repo, ctx.task.slug, (line) => log(ctx.task.id, line), ctx.branch);
    const args = ["-p", prompt, "--output-format", "stream-json", "--verbose", ...(this.cfg.args ?? [])];
    const child = spawn(this.cfg.command ?? "claude", args, { cwd: worktree, stdio: ["ignore", "pipe", "pipe"] });
    track(child, ctx.task.id);
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
            if (event.type === "system" && event.subtype === "init" && event.session_id) ctx.onSession?.(event.session_id);
            // Failed tool calls, refused permissions above all: otherwise the log goes quiet with no reason.
            for (const item of event.type === "user" ? event.message?.content ?? [] : []) {
              if (item.type === "tool_result" && item.is_error) log(ctx.task.id, `✗ ${toolError(item.content)}`);
            }
            for (const item of event.type === "assistant" ? event.message?.content ?? [] : []) {
              if (item.type === "text" && item.text?.trim()) log(ctx.task.id, `🤖 ${oneLine(item.text, 300)}`);
              if (item.type === "tool_use") log(ctx.task.id, `→ ${item.name}${toolHint(item.input)}`);
            }
            if (event.type === "result") {
              result = {
                kind: "completed",
                ok: !event.is_error && event.subtype === "success",
                summary: event.result ?? `Agent stopped: ${event.subtype}`,
                worktree,
                costUsd: event.total_cost_usd,
                sessionId: event.session_id,
                denied: (event.permission_denials ?? []).map((d: any) => `${d.tool_name}${toolHint(d.tool_input)}`),
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

function oneLine(text: string, max: number): string {
  const t = text.replace(/\s+/g, " ").trim();
  return t.length > max ? t.slice(0, max - 1) + "…" : t;
}

function toolError(content: unknown): string {
  const text = typeof content === "string" ? content : Array.isArray(content) ? content.map((c: any) => c?.text ?? "").join(" ") : "";
  if (/permission|haven't granted|not allowed|denied/i.test(text)) return `немає дозволу: ${oneLine(text, 200)}`;
  return oneLine(text || "помилка", 200);
}

/** "→ Bash: npm test" rather than the whole tool input. */
function toolHint(input: any): string {
  const v = input?.command ?? input?.file_path ?? input?.pattern ?? input?.path ?? input?.url;
  return typeof v === "string" ? `: ${oneLine(v, 160)}` : "";
}
