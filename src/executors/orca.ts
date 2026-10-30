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
  /**
   * What the agent in Orca is told to do with the task, after its description; the default has it finish with
   * a pushed branch and an MR, which is what Mako watches for. Placeholders: {branch} {baseBranch} {taskId}
   * {title} {url} {closingRef}.
   */
  instructions?: string;
  /** Who opens the MR: the agent (default), or the developer, who reviews and opens it with Orca's own button. */
  mrBy?: "agent" | "me";
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
      "--prompt", `${prompt}\n\n${handoffInstructions(this.cfg.instructions, ctx, this.cfg.mrBy)}`,
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

/** The end of the job for an agent in Orca: commit, push the task's branch and open the MR Mako waits for. */
export const DEFAULT_HANDOFF = `## Finish with a merge request
Take the task all the way to a merge request yourself:
1. Implement it and check it (tests, linters) as described above.
2. Commit on the branch {branch}. Each message: a first line under 72 characters that says what the change does,
   in the imperative ("Fix pagination size in the stubs admin"), starting with "{taskId}: "; then a blank line and
   1-5 bullets on what changed and why. No "fix", "wip" or "update" on their own.
3. Push the branch: git push -u origin {branch}
4. Open a merge request from {branch} into {baseBranch}. Title: what it does, in a sentence, not the branch name.
   Description in Markdown, short, from the actual diff:
   ## What changed
   - 2-6 bullets: what and why
   ## How it was checked
   - the tests and checks you ran, with results
   ## Worth a look
   - risks, open questions, anything left undone (skip if none)
   {closingRef}
   GitLab: \`glab mr create --target-branch {baseBranch} --fill\` when glab is installed, otherwise
   \`git push -o merge_request.create -o merge_request.target={baseBranch} -o merge_request.title="…" origin {branch}\`.
   GitHub: \`gh pr create --base {baseBranch} --fill\`.
5. Reply with the link to the merge request.
Mako watches the branch: once the merge request exists, it moves the issue on and comments with the link.`;

/** The developer opens the MR in Orca after a look: the agent stops at commits on the task's branch. */
export const DEFAULT_HANDOFF_FOR_ME = `## Finish with commits, not a merge request
1. Implement the task and check it (tests, linters) as described above.
2. Commit on the branch {branch}. Each message: a first line under 72 characters that says what the change does,
   in the imperative ("Fix pagination size in the stubs admin"), starting with "{taskId}: "; then a blank line and
   1-5 bullets on what changed and why. No "fix", "wip" or "update" on their own: Orca writes the merge request
   from these messages.
3. Do not push and do not open a merge request: the developer reviews the changes and opens it from Orca.
4. Reply with a ready merge request: a title (what it does, in a sentence) and a Markdown description with
   "## What changed", "## How it was checked", "## Worth a look", ending with "{closingRef}".`;

export function handoffInstructions(
  custom: string | undefined,
  ctx: Pick<ExecContext, "task" | "branch" | "repo">,
  mrBy: "agent" | "me" = "agent",
): string {
  const values: Record<string, string> = {
    branch: ctx.branch,
    baseBranch: ctx.repo.baseBranch,
    taskId: ctx.task.id,
    title: ctx.task.title,
    url: ctx.task.url,
    closingRef: ctx.task.closingRef ?? ctx.task.url,
  };
  return (custom?.trim() || (mrBy === "me" ? DEFAULT_HANDOFF_FOR_ME : DEFAULT_HANDOFF)).replace(/\{(\w+)\}/g, (all, key) => values[key] ?? all);
}

