import { mkdirSync, readFileSync, writeFileSync, existsSync, renameSync } from "node:fs";
import path from "node:path";

export type TaskStatus = "running" | "done" | "failed";

export interface TaskRecord {
  key: string;
  status: TaskStatus;
  startedAt: string;
  finishedAt?: string;
  worktree?: string;
  prUrl?: string;
  sessionId?: string;
  costUsd?: number;
  error?: string;
}

/** Remembers which issues were already picked up, so restarts don't redo work. */
export class StateStore {
  private file: string;
  private tasks: Record<string, TaskRecord> = {};

  constructor(private dir: string) {
    mkdirSync(path.join(dir, "logs"), { recursive: true });
    this.file = path.join(dir, "state.json");
    if (existsSync(this.file)) this.tasks = JSON.parse(readFileSync(this.file, "utf8"));
    // Tasks that were running when the process died are retried on the next poll.
    for (const t of Object.values(this.tasks)) if (t.status === "running") delete this.tasks[t.key];
    this.save();
  }

  get(key: string): TaskRecord | undefined {
    return this.tasks[key];
  }

  set(record: TaskRecord): void {
    this.tasks[record.key] = record;
    this.save();
  }

  logPath(key: string): string {
    return path.join(this.dir, "logs", `${key}.jsonl`);
  }

  private save(): void {
    const tmp = this.file + ".tmp";
    writeFileSync(tmp, JSON.stringify(this.tasks, null, 2));
    renameSync(tmp, this.file);
  }
}
