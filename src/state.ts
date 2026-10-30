import { mkdirSync, readFileSync, writeFileSync, existsSync, renameSync } from "node:fs";
import path from "node:path";
import type { ReviewInfo, Task } from "./types.js";

/**
 * asking   – notification is on screen, waiting for "Взяти" / "Відкласти"
 * snoozed  – postponed until `snoozedUntil`, then asked again
 * queued   – taken, waiting for a free agent slot
 * handed-off – passed to an interactive tool (Orca, a terminal); the developer finishes it there
 */
export type TaskStatus = "asking" | "snoozed" | "queued" | "running" | "handed-off" | "done" | "failed" | "cancelled";

export interface TaskRecord {
  id: string;
  status: TaskStatus;
  task: Task;
  /** Name of the local repo the task was routed to. */
  repo?: string;
  /** Name of the executor that ran the task. */
  executor?: string;
  /** Chosen by the developer when taking the task; win over routing and the default executor. */
  chosenRepo?: string;
  chosenExecutor?: string;
  /** Moved off the board by the developer; the record stays so the task is not offered again. */
  archivedAt?: number;
  updatedAt: string;
  snoozedUntil?: number;
  queuedAt?: number;
  startedAt?: string;
  finishedAt?: string;
  worktree?: string;
  /** Branch of a handed-off task, watched for a PR/MR. */
  branch?: string;
  branchSha?: string;
  prUrl?: string;
  sessionId?: string;
  /** Continue this agent conversation on the next run (claude --resume) instead of starting over. */
  resumeSession?: string;
  /** Tool calls the agent was refused in its last run; the developer has to allow them. */
  denied?: string[];
  /** The issue was closed or deleted in the tracker after Mako picked it up. */
  gone?: "closed" | "deleted";
  /** What happened to the issue's status in the tracker at the last step, shown on the card. */
  trackerStatus?: { ok: boolean; text: string };
  /** Commit the PR/MR was last pushed at, so finishing does not push (and reopen) an already merged branch. */
  publishedSha?: string;
  /** The issue already has Mako's comment about the PR/MR (posted when it was opened from the session). */
  reviewAnnounced?: boolean;
  /** Started over from scratch this many times: each attempt has its own slug, worktree and branch ("…-try2"). */
  attempt?: number;
  /** What happened to the PR/MR in the end: merged, or closed without merging. */
  reviewState?: "merged" | "closed";
  /** Last look at the PR/MR: CI, review threads, conflicts. */
  review?: ReviewInfo;
  /** The next run continues in the same worktree with this request instead of the full task prompt. */
  followUp?: string;
  /** The task's branch, named from the pattern when it first ran; kept for later runs and the MR. */
  branchName?: string;
  costUsd?: number;
  error?: string;
}

/** Remembers every task the agent has seen, so restarts neither re-ask nor redo work. */
export class StateStore {
  private file: string;
  private tasks: Record<string, TaskRecord> = {};

  constructor(private dir: string) {
    mkdirSync(path.join(dir, "logs"), { recursive: true });
    this.file = path.join(dir, "state.json");
    if (existsSync(this.file)) this.tasks = JSON.parse(readFileSync(this.file, "utf8"));
    // A process that died mid-question asks again; a task that was running is picked up again.
    for (const t of Object.values(this.tasks)) {
      if (t.status === "asking") Object.assign(t, { status: "snoozed", snoozedUntil: 0 });
      if (t.status === "running") Object.assign(t, { status: "queued", queuedAt: 0 });
    }
    this.save();
  }

  get(id: string): TaskRecord | undefined {
    return this.tasks[id];
  }

  all(): TaskRecord[] {
    return Object.values(this.tasks);
  }

  update(id: string, patch: Partial<TaskRecord> & { task?: Task }): TaskRecord {
    const current = this.tasks[id];
    if (!current && !patch.task) throw new Error(`Unknown task ${id}`);
    this.tasks[id] = { ...current, ...patch, id, updatedAt: new Date().toISOString() } as TaskRecord;
    this.save();
    return this.tasks[id];
  }

  delete(id: string): void {
    delete this.tasks[id];
    this.save();
  }

  logPath(task: Task): string {
    return path.join(this.dir, "logs", `${task.trackerId}-${task.slug}.jsonl`);
  }

  private save(): void {
    const tmp = this.file + ".tmp";
    writeFileSync(tmp, JSON.stringify(this.tasks, null, 2));
    renameSync(tmp, this.file);
  }
}
