import { appendFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { run } from "../shell.js";
import { findCommand } from "./find-command.js";
import { log } from "../log.js";
import type { RepoConfig } from "../config.js";
import type { ExecContext, ExecOutcome, Executor, WorkspaceStatus } from "./types.js";

export interface OrcaExecutorConfig {
  type: "orca";
  label?: string;
  /** Path to the Orca CLI. */
  command?: string;
  /** Orca agent id: claude, codex, cursor, opencode, … */
  agent?: string;
  /** Repo setup hooks policy. */
  setup?: "run" | "skip" | "inherit";
  /** Link the tracker issue to the Orca workspace (`--reference <url>`). */
  linkIssue?: boolean;
  extraArgs?: string[];
}

/**
 * Hands the task to Orca: it creates the worktree, opens a terminal with the agent and the prompt,
 * and the developer follows and steers the session in Orca. The repo must be added to Orca.
 */
export class OrcaExecutor implements Executor {
  readonly label: string;
  readonly interactive = true;
  readonly managed = false;

  constructor(readonly name: string, private cfg: OrcaExecutorConfig) {
    this.label = cfg.label ?? "Orca";
  }

  async run(ctx: ExecContext, prompt: string): Promise<ExecOutcome> {
    const base = [
      "worktree", "create",
      "--repo", `path:${ctx.repo.repoPath}`,
      "--name", ctx.task.slug,
      "--branch", ctx.branch,
      "--base-branch", ctx.repo.baseBranch,
      "--agent", this.cfg.agent ?? "claude",
      "--prompt", prompt,
      "--setup", this.cfg.setup ?? "inherit",
      "--no-parent",
      "--json",
      ...(this.cfg.extraArgs ?? []),
    ];
    const command = resolveOrca(this.cfg.command);
    const withLink = this.cfg.linkIssue === false ? base : [...base, "--reference", ctx.task.url];
    const orca = (args: string[]) =>
      run(command, args, ctx.repo.repoPath).catch((err) => {
        throw new Error(`Не вдалося запустити ${command}: ${err.message}`);
      });

    await ensureOrcaRunning(command, ctx);
    let r = await orca(withLink);
    let failure = r.code === 0 ? undefined : orcaError(r.stdout || r.stderr);

    // Only a complaint about the issue link is worth a second try without it; any other failure may
    // already have started an agent, and launching again would make a duplicate workspace.
    if (failure && withLink !== base && failure.code !== UNKNOWN_OUTCOME && /reference|link|url|issue source/i.test(failure.message)) {
      r = await orca(base);
      failure = r.code === 0 ? undefined : orcaError(r.stdout || r.stderr);
    }

    if (failure?.code === UNKNOWN_OUTCOME) {
      // Orca lost the reply mid-launch: the agent may be running. Replay the launch the way Orca says is safe,
      // then look for the workspace before calling it a failure.
      const replay = failure.data?.recoveryCommand;
      if (failure.data?.retrySafe && Array.isArray(replay) && replay.length > 1) {
        r = await run(replay[0], replay.slice(1), ctx.repo.repoPath).catch(() => r);
        failure = r.code === 0 ? undefined : orcaError(r.stdout || r.stderr);
      }
      if (failure) {
        const existing = await findWorkspace(command, ctx);
        if (existing) {
          await this.markWorkspace(ctx.repo, { worktree: existing.path, branch: existing.branch }, "in-progress").catch((err) => log(ctx.task.id, err.message));
          return {
            kind: "handed-off",
            worktree: existing.path,
            branch: existing.branch,
            details: `Orca workspace ${ctx.task.slug}; Orca не підтвердила, що промпт дійшов до агента, перевірте його в Orca`,
          };
        }
      }
    }
    if (failure) {
      // Keep Orca's full answer for diagnosis; the inbox shows the readable part.
      appendFileSync(ctx.logPath, `\n--- orca ${new Date().toISOString()}\n${(r.stdout + r.stderr).trim()}\n`);
      throw new Error(`${explain(failure)}\nПовна відповідь Orca: ${ctx.logPath}`);
    }

    let worktree: string | undefined;
    let branch = ctx.branch;
    try {
      const json = JSON.parse(r.stdout);
      const wt = json.worktree ?? json.result?.worktree ?? json;
      worktree = wt.path;
      // Orca suffixes the branch (agent/X-1-2) when the name is taken.
      branch = (wt.branch ?? branch).replace(/^refs\/heads\//, "");
    } catch {
      // Non-JSON output is fine; the workspace exists.
    }
    await this.markWorkspace(ctx.repo, { worktree, branch }, "in-progress").catch((err) => log(ctx.task.id, err.message));
    return {
      kind: "handed-off",
      worktree,
      branch,
      details: `Orca workspace ${ctx.task.slug}, agent ${this.cfg.agent ?? "claude"}`,
    };
  }

  /** Moves the workspace to a column of Orca's board (default column ids). */
  async markWorkspace(repo: RepoConfig, where: { worktree?: string; branch: string }, status: WorkspaceStatus): Promise<void> {
    const selector = where.worktree ? `path:${where.worktree}` : `branch:${where.branch}`;
    const r = await run(resolveOrca(this.cfg.command), ["worktree", "set", "--worktree", selector, "--workspace-status", status, "--json"], repo.repoPath);
    if (r.code !== 0) throw new Error(`Orca: статус workspace «${status}» не встановлено: ${orcaError(r.stdout || r.stderr).message}`);
  }
}

/**
 * The Orca CLI ships inside the app and reaches PATH only after Orca → Settings → General → Orca CLI;
 * look inside the app too. On Linux the command is orca-ide (GNOME's screen reader owns `orca`).
 */
/** The Orca CLI on PATH or inside Orca.app, if Orca is installed. */
export function findOrca(): string | undefined {
  const inApp = (app: string) => path.join(app, "Orca.app/Contents/Resources/bin/orca");
  return process.platform === "linux"
    ? findCommand("orca-ide")
    : findCommand("orca", [inApp("/Applications"), inApp(path.join(os.homedir(), "Applications"))]);
}

function resolveOrca(configured: string | undefined): string {
  if (configured) {
    const found = findCommand(configured);
    if (found) return found;
    throw new Error(`Orca CLI не знайдено за шляхом «${configured}». Перевірте «Команда» у виконавця Orca в Налаштуваннях.`);
  }
  if (process.platform === "win32") return "orca";
  const found = findOrca();
  if (found) return found;
  throw new Error(
    "Orca CLI не знайдено. Встановіть Orca (onorca.dev) і зареєструйте CLI: Orca → Settings → General → Orca CLI. " +
      "Або вкажіть повний шлях у Налаштуваннях Mako → «Хто виконує задачі» → Orca → «Команда». " +
      "Якщо Orca вам не потрібна, виберіть для репозиторію інший виконавець, наприклад «Термінал у Mako».",
  );
}

/**
 * The CLI talks to the Orca app, so a closed app fails every hand-off. On macOS Mako opens Orca and waits
 * (up to a minute) until the CLI answers; elsewhere it says to open it.
 */
async function ensureOrcaRunning(command: string, ctx: ExecContext): Promise<void> {
  const answers = async () =>
    (await run(command, ["worktree", "list", "--repo", `path:${ctx.repo.repoPath}`, "--json"], ctx.repo.repoPath).catch(() => undefined))?.code === 0;
  if (await answers()) return;
  if (process.platform !== "darwin") {
    if (await appRunning()) return; // running, but something else is wrong: the hand-off reports it
    throw new Error("Orca не запущена. Відкрийте Orca і натисніть «Повторити».");
  }
  if (await appRunning()) return;
  log(ctx.task.id, "Orca не запущена: відкриваю Orca і чекаю, поки вона підніметься");
  const opened = await run("open", ["-a", "Orca"], ctx.repo.repoPath).catch(() => undefined);
  if (opened?.code !== 0) throw new Error("Orca не запущена, і Mako не зміг її відкрити. Відкрийте Orca і натисніть «Повторити».");
  for (let i = 0; i < 30; i++) {
    await new Promise((r) => setTimeout(r, 2000));
    if (await answers()) {
      log(ctx.task.id, "Orca запущена");
      return;
    }
  }
  // Up but not answering for this repo (not added in Orca?): the hand-off itself says what is wrong.
}

async function appRunning(): Promise<boolean> {
  // The app's own process name, not any command line that mentions it.
  const name = process.platform === "darwin" ? "Orca" : "orca-ide";
  return (await run("pgrep", ["-x", name], os.homedir()).catch(() => undefined))?.code === 0;
}

const UNKNOWN_OUTCOME = "agent_session_operation_unknown";

interface OrcaFailure {
  code?: string;
  message: string;
  data?: { cause?: string; nextSteps?: string[]; recoveryCommand?: string[]; retrySafe?: boolean };
}

/** Orca's --json errors: { ok: false, error: { code, message, data } }; plain text otherwise. */
function orcaError(output: string): OrcaFailure {
  const text = output.trim();
  try {
    const json = JSON.parse(text.slice(text.indexOf("{")));
    if (json?.error) return { code: json.error.code, message: String(json.error.message ?? ""), data: json.error.data };
  } catch {
    // not JSON
  }
  return { message: text || "orca завершився з помилкою" };
}

function explain(f: OrcaFailure): string {
  const lines = [
    f.code === UNKNOWN_OUTCOME
      ? "Orca не змогла підтвердити запуск агента, а workspace задачі не знайдено."
      : `Orca не створила workspace${f.code ? ` (${f.code})` : ""}: ${f.message}`,
  ];
  // A cause that only repeats an error code says nothing; Orca's own next steps say more.
  const cause = f.data?.cause?.trim();
  if (cause && !/^[a-z_]+$/.test(cause)) lines.push(`Причина від Orca: ${cause}`);
  if (f.data?.nextSteps?.length) lines.push(...f.data.nextSteps.map((s) => `Orca радить: ${s}`));
  if (f.code === UNKNOWN_OUTCOME) {
    lines.push(
      "Перевірте: застосунок Orca запущений і оновлений до останньої версії (CLI і застосунок мають збігатися), " +
        "а цей репозиторій доданий в Orca. Потім натисніть «Повторити».",
    );
  }
  return lines.join("\n");
}

/** The task's workspace in Orca, if it exists: by branch, including Orca's numeric suffixes (agent/X-1-2). */
async function findWorkspace(command: string, ctx: ExecContext): Promise<{ path?: string; branch: string } | undefined> {
  const r = await run(command, ["worktree", "list", "--repo", `path:${ctx.repo.repoPath}`, "--json"], ctx.repo.repoPath).catch(() => undefined);
  if (!r || r.code !== 0) return undefined;
  let json: unknown;
  try {
    json = JSON.parse(r.stdout);
  } catch {
    return undefined;
  }
  const matches = (b: unknown) =>
    typeof b === "string" && (b.replace(/^refs\/heads\//, "") === ctx.branch || new RegExp(`^${ctx.branch.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}-\\d+$`).test(b.replace(/^refs\/heads\//, "")));
  // The list's exact shape varies between Orca versions: look for any object with a matching branch.
  const stack: unknown[] = [json];
  while (stack.length) {
    const node = stack.pop();
    if (Array.isArray(node)) stack.push(...node);
    else if (node && typeof node === "object") {
      const o = node as Record<string, unknown>;
      if (matches(o.branch)) return { path: typeof o.path === "string" ? o.path : undefined, branch: String(o.branch).replace(/^refs\/heads\//, "") };
      stack.push(...Object.values(o));
    }
  }
  return undefined;
}
