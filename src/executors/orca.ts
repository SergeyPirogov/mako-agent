import { run } from "../shell.js";
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
    const command = this.cfg.command ?? "orca";
    const withLink = this.cfg.linkIssue === false ? base : [...base, "--reference", ctx.task.url];

    let r = await run(command, withLink, ctx.repo.repoPath).catch((err) => {
      throw new Error(`Cannot run ${command}: ${err.message}. Is the Orca CLI installed and on PATH?`);
    });
    if (r.code !== 0 && withLink !== base) {
      // Older Orca hosts may not know this issue source; the workspace matters more than the link.
      r = await run(command, base, ctx.repo.repoPath);
    }
    if (r.code !== 0) throw new Error(`orca worktree create failed: ${(r.stderr || r.stdout).trim()}`);

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
