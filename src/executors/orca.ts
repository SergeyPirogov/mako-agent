import os from "node:os";
import path from "node:path";
import { run } from "../shell.js";
import { findCommand } from "./find-command.js";
import type { ExecContext, ExecOutcome, Executor } from "./types.js";

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
          return {
            kind: "handed-off",
            worktree: existing.path,
            branch: existing.branch,
            details: `Orca workspace ${ctx.task.slug}; Orca не підтвердила, що промпт дійшов до агента, перевірте його в Orca`,
          };
        }
      }
    }
    if (failure) throw new Error(explain(failure));

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
    return {
      kind: "handed-off",
      worktree,
      branch,
      details: `Orca workspace ${ctx.task.slug}, agent ${this.cfg.agent ?? "claude"}`,
    };
  }
}

/**
 * The Orca CLI ships inside the app and reaches PATH only after Orca → Settings → General → Orca CLI;
 * look inside the app too. On Linux the command is orca-ide (GNOME's screen reader owns `orca`).
 */
function resolveOrca(configured: string | undefined): string {
  if (configured) {
    const found = findCommand(configured);
    if (found) return found;
    throw new Error(`Orca CLI не знайдено за шляхом «${configured}». Перевірте «Команда» у виконавця Orca в Налаштуваннях.`);
  }
  if (process.platform === "win32") return "orca";
  const home = os.homedir();
  const inApp = (app: string) => path.join(app, "Orca.app/Contents/Resources/bin/orca");
  const found =
    process.platform === "linux"
      ? findCommand("orca-ide")
      : findCommand("orca", [inApp("/Applications"), inApp(path.join(home, "Applications"))]);
  if (found) return found;
  throw new Error(
    "Orca CLI не знайдено. Встановіть Orca (onorca.dev) і зареєструйте CLI: Orca → Settings → General → Orca CLI. " +
      "Або вкажіть повний шлях у Налаштуваннях Mako → «Хто виконує задачі» → Orca → «Команда». " +
      "Якщо Orca вам не потрібна, виберіть для репозиторію інший виконавець, наприклад «Термінал у Mako».",
  );
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
  if (f.data?.cause) lines.push(`Причина від Orca: ${f.data.cause.trim()}`);
  if (f.code === UNKNOWN_OUTCOME) lines.push("Перевірте, що застосунок Orca запущений, і натисніть «Повторити».");
  else if (f.data?.nextSteps?.length) lines.push(...f.data.nextSteps);
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
