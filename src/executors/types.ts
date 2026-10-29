import type { RepoConfig } from "../config.js";
import type { Task, Tracker } from "../types.js";

export interface ExecContext {
  task: Task;
  tracker: Tracker;
  repo: RepoConfig;
  branch: string;
  /** Where the executor may write its log. */
  logPath: string;
  /** Prompt also saved to a file, for tools that read it from disk. */
  promptFile: string;
  timeoutMin: number;
}

/**
 * completed  – Mako waited for the agent; it commits, pushes, opens the PR and reports back.
 * handed-off – the task now lives in another tool (Orca, a terminal); the developer drives it from there.
 */
export type ExecOutcome =
  | { kind: "completed"; ok: boolean; summary: string; worktree: string; costUsd?: number; sessionId?: string }
  | { kind: "handed-off"; worktree?: string; details?: string };

export interface Executor {
  /** Key from the `executors` config. */
  name: string;
  /** Human-readable name for notifications and comments: "Claude Code", "Orca". */
  label: string;
  /** Whether the developer sees and steers the session (changes the prompt wording). */
  interactive: boolean;
  /** Mako waits for the result; the run occupies one `maxConcurrent` slot. */
  managed: boolean;
  run(ctx: ExecContext, prompt: string): Promise<ExecOutcome>;
}
