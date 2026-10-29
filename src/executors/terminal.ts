import { randomBytes } from "node:crypto";
import { accessSync, chmodSync, constants, createWriteStream, existsSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { spawn as spawnPty } from "node-pty";
import * as git from "../git.js";
import { Session, sessions, sessionIdFor, plainTail } from "../sessions.js";
import { inform } from "../notify.js";
import { log } from "../log.js";
import { expandArgs } from "./command.js";
import { onShutdown } from "./processes.js";
import type { ExecContext, ExecOutcome, Executor } from "./types.js";

export interface TerminalExecutorConfig {
  type: "terminal";
  label?: string;
  /** Interactive agent CLI; on Windows npm shims need the .cmd suffix (claude.cmd). */
  command?: string;
  /** Placeholders as in `command`; default: ["{prompt}"]. */
  args?: string[];
  /** Report working / waiting / done through Claude Code hooks. Defaults to true when the command is claude. */
  claudeHooks?: boolean;
  /** Finish by itself when the agent ends its turn and nobody typed into the terminal. */
  autoFinish?: boolean;
}

/**
 * Runs an interactive agent (Claude Code by default) in a pseudo-terminal that the Mako web UI shows live.
 * The developer can watch, answer, approve or redirect at any moment; "Завершити" ends the session and
 * Mako then commits, pushes and opens the PR as with the headless executor.
 */
export class TerminalExecutor implements Executor {
  readonly label: string;
  readonly interactive = true;
  readonly managed = true;
  private isClaude: boolean;

  constructor(readonly name: string, private cfg: TerminalExecutorConfig) {
    const command = cfg.command ?? "claude";
    this.isClaude = /^claude(\.cmd|\.exe)?$/i.test(path.basename(command));
    this.label = cfg.label ?? (this.isClaude ? "Claude Code" : command);
  }

  async run(ctx: ExecContext, prompt: string): Promise<ExecOutcome> {
    if (!ctx.ui) throw new Error("The terminal executor shows sessions in the web UI: set notifications.inboxPort");
    const worktree = await git.prepareWorktree(ctx.repo, ctx.task.slug, (line) => log(ctx.task.id, line));
    const id = sessionIdFor(ctx.task);
    const hookToken = randomBytes(16).toString("hex");
    const sessionUrl = `http://127.0.0.1:${ctx.ui.port}/session/${id}`;

    const args = expandArgs(this.cfg.args ?? ["{prompt}"], ctx, worktree, prompt);
    if (this.cfg.claudeHooks ?? this.isClaude) {
      const settingsFile = ctx.promptFile.replace(/\.prompt\.md$/, ".claude-settings.json");
      writeFileSync(settingsFile, JSON.stringify(claudeHookSettings(`http://127.0.0.1:${ctx.ui.port}/hook/${id}?token=${hookToken}`)), { mode: 0o600 });
      args.unshift("--settings", settingsFile);
    }

    ensureSpawnHelper();
    const command = resolveCommand(this.cfg.command ?? "claude");
    const pty = spawnPty(command, args, {
      name: "xterm-256color",
      cols: 120,
      rows: 34,
      cwd: worktree,
      env: { ...process.env, MAKO_TASK: ctx.task.id, FORCE_COLOR: "1" } as Record<string, string>,
    });
    const session = new Session(
      id,
      { id: ctx.task.id, title: ctx.task.title, url: ctx.task.url },
      this.label,
      hookToken,
      pty,
      this.isClaude ? "/exit" : undefined,
    );
    sessions.get(id)?.kill();
    sessions.set(id, session);
    const off = onShutdown(() => session.kill());
    const logFile = createWriteStream(ctx.logPath.replace(/\.jsonl$/, ".terminal.log"), { flags: "a" });
    log(ctx.task.id, `terminal session: ${sessionUrl}`);
    inform(`🦈 ${ctx.task.id}: агент почав`, `${this.label} працює в терміналі Mako`, sessionUrl);

    return new Promise((resolve) => {
      // Notify when the agent needs the developer, and keep reminding until they react.
      let reminder: NodeJS.Timeout | undefined;
      const notifyAttention = (repeat: boolean) => {
        if (repeat && session.acknowledged) return; // the developer is already typing in this session
        const what = session.status === "waiting" ? "агент чекає на вас" : "агент закінчив, перевірте";
        const minutes = session.attentionSince ? Math.round((Date.now() - session.attentionSince) / 60_000) : 0;
        inform(`${repeat ? "⏰" : "🦈"} ${ctx.task.id}: ${what}${repeat ? ` (${minutes} хв)` : ""}`, session.message ?? ctx.task.title, sessionUrl);
      };
      session.subscribe((e) => {
        if (e.t === "o") logFile.write(e.d);
        if (e.t === "s") {
          clearInterval(reminder);
          reminder = undefined;
          if (e.status === "waiting" || e.status === "idle") {
            if (e.status === "idle" && this.cfg.autoFinish && !session.userTouched) return session.finish();
            notifyAttention(false);
            const every = ctx.ui!.remindMin;
            if (every > 0) reminder = setInterval(() => notifyAttention(true), every * 60_000);
          }
        }
        if (e.t === "x") {
          clearInterval(reminder);
          off();
          logFile.end();
          const cancelled = session.ending === "cancel";
          const ok = !cancelled && (session.ending === "finish" || e.code === 0);
          const summary = cancelled
            ? "Session cancelled by the developer."
            : session.lastAssistantMessage ?? (plainTail(session.output) || `Session ended with code ${e.code}`);
          resolve({ kind: "completed", ok, summary, worktree });
        }
      });
    });
  }
}

/** Hooks that report Claude's state to Mako; merged by Claude Code with the user's own settings. */
function claudeHookSettings(url: string) {
  const post = { type: "command", command: `curl -s -m 3 -X POST -H "Content-Type: application/json" --data-binary @- "${url}"` };
  return {
    hooks: {
      UserPromptSubmit: [{ hooks: [post] }],
      PreToolUse: [{ matcher: "*", hooks: [post] }],
      // After a permission is granted the tool runs: this moves the status back from "waiting" to "working".
      PostToolUse: [{ matcher: "*", hooks: [post] }],
      Notification: [{ hooks: [post] }],
      Stop: [{ hooks: [post] }],
    },
  };
}

/**
 * node-pty starts processes through its spawn-helper; the macOS prebuilds ship it without the execute bit,
 * which fails every session with "posix_spawnp failed". scripts/postinstall.cjs fixes it on install;
 * this covers copies installed before that and packaged apps.
 */
let helperChecked = false;
function ensureSpawnHelper(): void {
  if (helperChecked || process.platform === "win32") return;
  helperChecked = true;
  try {
    const root = path.dirname(createRequire(import.meta.url).resolve("node-pty/package.json")).replace("app.asar", "app.asar.unpacked");
    const dirs = ["build/Release", ...(existsSync(path.join(root, "prebuilds")) ? readdirSync(path.join(root, "prebuilds")).map((d) => `prebuilds/${d}`) : [])];
    for (const dir of dirs) {
      const helper = path.join(root, dir, "spawn-helper");
      if (existsSync(helper) && !(statSync(helper).mode & 0o111)) chmodSync(helper, 0o755);
    }
  } catch (err: any) {
    log("terminal", `cannot fix node-pty spawn-helper permissions: ${err.message}`);
  }
}

/**
 * Finds the agent binary: as given if it is a path, otherwise in PATH and the usual install folders
 * (apps started from Finder see a minimal PATH). A missing binary otherwise also shows up as "posix_spawnp failed".
 */
function resolveCommand(command: string): string {
  if (process.platform === "win32") return command;
  const home = os.homedir();
  const expanded = command.startsWith("~/") ? path.join(home, command.slice(2)) : command;
  if (expanded.includes("/")) {
    if (isExecutable(expanded)) return expanded;
    throw new Error(`Команда агента не знайдена або не виконувана: ${expanded}`);
  }
  const dirs = [
    ...(process.env.PATH ?? "").split(path.delimiter),
    path.join(home, ".local/bin"),
    path.join(home, ".claude/local"),
    path.join(home, ".npm-global/bin"),
    path.join(home, ".bun/bin"),
    path.join(home, ".volta/bin"),
    "/opt/homebrew/bin",
    "/usr/local/bin",
  ].filter(Boolean);
  for (const dir of dirs) {
    const candidate = path.join(dir, expanded);
    if (isExecutable(candidate)) return candidate;
  }
  throw new Error(
    `Не знайдено команду «${command}». Перевірте в терміналі: which ${command}, і вкажіть повний шлях ` +
      `у Налаштуваннях → «Хто виконує задачі» → «Команда агента» (наприклад ${path.join(home, ".local/bin", command)}).`,
  );
}

function isExecutable(file: string): boolean {
  try {
    accessSync(file, constants.X_OK);
    return statSync(file).isFile();
  } catch {
    return false;
  }
}
