import { spawn } from "node:child_process";
import { createWriteStream } from "node:fs";
import * as git from "../git.js";
import { log } from "../log.js";
import { track } from "./processes.js";
import type { ExecContext, ExecOutcome, Executor } from "./types.js";

export interface CommandExecutorConfig {
  type: "command";
  label?: string;
  /**
   * managed – wait for the command; exit code 0 means success, the stdout tail becomes the summary.
   * handoff – start it detached (a terminal, an IDE, another agent UI) and let the developer take over.
   */
  mode?: "managed" | "handoff";
  /** Whether the developer watches the session; defaults to true for handoff. */
  interactive?: boolean;
  command: string;
  /** Placeholders: {worktree} {prompt} {promptFile} {taskId} {slug} {branch} {title} {url} {repoPath} {repo} */
  args?: string[];
  /** Create a git worktree first (default true). Turn off when the tool makes its own. */
  worktree?: boolean;
}

/** Runs any CLI as the agent: codex, aider, a script, `tmux new-window …`, `code …`. */
export class CommandExecutor implements Executor {
  readonly label: string;
  readonly interactive: boolean;
  readonly managed: boolean;

  constructor(readonly name: string, private cfg: CommandExecutorConfig) {
    this.label = cfg.label ?? cfg.command;
    this.managed = (cfg.mode ?? "managed") === "managed";
    this.interactive = cfg.interactive ?? !this.managed;
  }

  async run(ctx: ExecContext, prompt: string): Promise<ExecOutcome> {
    const worktree = this.cfg.worktree === false ? ctx.repo.repoPath : await git.prepareWorktree(ctx.repo, ctx.task.slug, (line) => log(ctx.task.id, line));
    const args = expandArgs(this.cfg.args ?? [], ctx, worktree, prompt);

    if (!this.managed) {
      const child = spawn(this.cfg.command, args, { cwd: worktree, detached: true, stdio: "ignore" });
      await new Promise<void>((resolve, reject) => {
        child.once("spawn", resolve);
        child.once("error", (err) => reject(new Error(`Failed to start ${this.cfg.command}: ${err.message}`)));
      });
      child.unref();
      return { kind: "handed-off", worktree: this.cfg.worktree === false ? undefined : worktree, branch: ctx.branch };
    }

    const child = spawn(this.cfg.command, args, { cwd: worktree, stdio: ["ignore", "pipe", "pipe"] });
    track(child);
    const logFile = createWriteStream(ctx.logPath, { flags: "a" });
    let output = "";
    const collect = (d: Buffer) => {
      logFile.write(d);
      output = (output + d).slice(-4000);
    };
    child.stdout.on("data", collect);
    child.stderr.on("data", collect);
    const timer = setTimeout(() => child.kill("SIGTERM"), ctx.timeoutMin * 60_000);

    return new Promise((resolve) => {
      const done = (ok: boolean, summary: string) => {
        clearTimeout(timer);
        logFile.end();
        resolve({ kind: "completed", ok, summary, worktree });
      };
      child.on("error", (err) => done(false, `Failed to start ${this.cfg.command}: ${err.message}`));
      child.on("close", (code, signal) =>
        done(code === 0, code === 0 ? output.trim() || "Done." : `Exited with ${signal ?? `code ${code}`}:\n${output.trim()}`),
      );
    });
  }
}

/** Fills {placeholders} in executor arguments. */
export function expandArgs(args: string[], ctx: ExecContext, worktree: string, prompt: string): string[] {
  const vars: Record<string, string> = {
    worktree,
    prompt,
    promptFile: ctx.promptFile,
    taskId: ctx.task.id,
    slug: ctx.task.slug,
    branch: ctx.branch,
    title: ctx.task.title,
    url: ctx.task.url,
    repoPath: ctx.repo.repoPath,
    repo: ctx.repo.name,
  };
  return args.map((a) => a.replace(/\{(\w+)\}/g, (m, k) => vars[k] ?? m));
}
