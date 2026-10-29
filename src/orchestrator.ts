import type { Config } from "./config.js";
import type { Task, Tracker } from "./types.js";
import { StateStore, type TaskRecord } from "./state.js";
import { buildPrompt } from "./prompt.js";
import { createExecutor, type Executor } from "./executors/index.js";
import type { RepoConfig } from "./config.js";
import { writeFileSync } from "node:fs";
import { askToTake, inform, type Decision } from "./notify.js";
import * as git from "./git.js";
import { routeTask } from "./routing.js";
import { log } from "./log.js";

export class Orchestrator {
  private trackers: Map<string, Tracker>;
  private executors: Map<string, Executor>;
  private active = new Map<string, Promise<void>>();
  /** Unroutable tasks already reported, so the log is not repeated on every poll. */
  private unroutable = new Map<string, string>();
  private stopped = false;

  constructor(
    private cfg: Config,
    trackers: Tracker[],
    readonly state: StateStore,
  ) {
    this.trackers = new Map(trackers.map((t) => [t.id, t]));
    this.executors = new Map(Object.entries(cfg.executors).map(([name, c]) => [name, createExecutor(name, c)]));
  }

  private executorFor(repo: RepoConfig): Executor {
    return this.executors.get(repo.executor ?? this.cfg.defaultExecutor)!;
  }

  async start(): Promise<void> {
    log("main", `Polling ${[...this.trackers.values()].map((t) => t.id).join(", ")} every ${this.cfg.pollIntervalSec}s`);
    this.schedule(); // tasks queued before a restart
    while (!this.stopped) {
      await this.poll();
      await new Promise((r) => setTimeout(r, this.cfg.pollIntervalSec * 1000));
    }
  }

  stop(): void {
    this.stopped = true;
  }

  async drain(): Promise<void> {
    await Promise.allSettled(this.active.values());
  }

  async poll(): Promise<void> {
    for (const tracker of this.trackers.values()) {
      let tasks: Task[];
      try {
        tasks = await tracker.fetchTasks();
      } catch (err: any) {
        log(tracker.id, `poll failed: ${err.message}`);
        continue;
      }
      const seen = new Set<string>();
      for (const task of tasks) {
        seen.add(task.id);
        const route = routeTask(task, tracker.projects[task.project]);
        if (!route.repo) {
          if (this.unroutable.get(task.id) !== route.reason) log(task.id, `skipped: ${route.reason}`);
          this.unroutable.set(task.id, route.reason);
          continue;
        }
        this.unroutable.delete(task.id);
        if (this.cfg.dryRun) {
          const ex = this.executorFor(route.repo);
          log(task.id, `[dry-run] → ${route.repo.name} (${route.repo.repoPath}) by ${route.reason}, executor ${ex.name}; prompt:\n${buildPrompt(task, tracker, route.repo, ex.interactive)}\n`);
          continue;
        }
        this.onTaskSeen(task, tracker, route.repo.name);
      }
      // A postponed task that left the query (closed, reassigned) no longer needs an answer.
      for (const rec of this.state.all()) {
        if (rec.task.trackerId === tracker.id && rec.status === "snoozed" && !seen.has(rec.id)) {
          log(rec.id, "no longer matches the tracker query, forgetting it");
          this.state.delete(rec.id);
        }
      }
    }
    this.schedule();
  }

  private onTaskSeen(task: Task, tracker: Tracker, repo: string): void {
    const rec = this.state.get(task.id);
    if (!rec) {
      log(task.id, `new task → ${repo}: ${task.title}`);
      this.state.update(task.id, { task, repo, status: "snoozed", snoozedUntil: 0 });
    } else if (rec.status === "snoozed" || rec.status === "queued") {
      this.state.update(task.id, { task, repo }); // keep the freshest description, comments and routing
    }
    const current = this.state.get(task.id)!;
    if (current.status === "snoozed" && (current.snoozedUntil ?? 0) <= Date.now()) this.ask(current, tracker);
  }

  private ask(rec: TaskRecord, tracker: Tracker): void {
    const n = this.cfg.notifications;
    if (n.autoTake || !n.enabled) {
      if (n.autoTake) return void this.decide(rec.id, "take");
      return; // notifications off: tasks wait in the inbox page
    }
    this.state.update(rec.id, { status: "asking" });
    askToTake(rec.task, tracker, this.routeLabel(rec), n).then((d) => this.decide(rec.id, d));
  }

  private routeLabel(rec: TaskRecord): string | undefined {
    const tracker = this.trackers.get(rec.task.trackerId);
    const repo = rec.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    return repo ? `${repo.name} · ${this.executorFor(repo).label}` : rec.repo;
  }

  /** Applies an answer from a notification or the inbox page. The first answer wins. */
  decide(id: string, decision: Decision): boolean {
    const rec = this.state.get(id);
    if (!rec || !["asking", "snoozed"].includes(rec.status)) return false;
    if (decision === "take") {
      log(id, "taken");
      this.state.update(id, { status: "queued", queuedAt: Date.now() });
      this.schedule();
    } else {
      const snoozeMin = this.cfg.notifications.snoozeMin;
      log(id, `${decision === "later" ? "postponed" : "no answer"}, asking again in ${snoozeMin} min`);
      this.state.update(id, { status: "snoozed", snoozedUntil: Date.now() + snoozeMin * 60_000 });
    }
    return true;
  }

  private schedule(): void {
    if (this.stopped) return;
    const queued = this.state
      .all()
      .filter((r) => r.status === "queued" && !this.active.has(r.id))
      .sort((a, b) => (a.queuedAt ?? 0) - (b.queuedAt ?? 0));
    for (const rec of queued) {
      if (this.active.size >= this.cfg.maxConcurrent) break;
      const job = this.execute(rec).finally(() => {
        this.active.delete(rec.id);
        this.schedule();
      });
      this.active.set(rec.id, job);
    }
  }

  private async execute(rec: TaskRecord): Promise<void> {
    const { task } = rec;
    const tracker = this.trackers.get(task.trackerId);
    const project = tracker?.projects[task.project];
    const repo = (rec.repo && project?.repos[rec.repo]) || routeTask(task, project).repo;
    if (!tracker || !repo) {
      this.state.update(task.id, { status: "failed", error: `no configured repository for ${task.id}` });
      return;
    }
    const executor = this.executorFor(repo);
    const branch = git.branchFor(task.slug);
    const prompt = buildPrompt(task, tracker, repo, executor.interactive);
    const promptFile = this.state.logPath(task).replace(/\.jsonl$/, ".prompt.md");
    writeFileSync(promptFile, prompt);
    this.state.update(task.id, {
      status: "running",
      startedAt: new Date().toISOString(),
      repo: repo.name,
      executor: executor.name,
    });

    try {
      await tracker.onStart(task);
      log(task.id, `starting ${executor.name} (${executor.label}) for repo ${repo.name}`);
      const result = await executor.run(
        { task, tracker, repo, branch, logPath: this.state.logPath(task), promptFile, timeoutMin: this.cfg.taskTimeoutMin },
        prompt,
      );

      if (result.kind === "handed-off") {
        const where = result.details ?? executor.label;
        await tracker.comment(task, `🦈 Mako handed this issue to ${executor.label} (${where}).`);
        this.state.update(task.id, { status: "handed-off", finishedAt: new Date().toISOString(), worktree: result.worktree });
        log(task.id, `handed off to ${executor.label}${result.worktree ? ` at ${result.worktree}` : ""}`);
        inform(`🦈 ${task.id} відкрито в ${executor.label}`, task.title);
        return;
      }

      const { worktree } = result;
      this.state.update(task.id, { worktree });
      log(task.id, `agent finished: ok=${result.ok}${result.costUsd !== undefined ? `, cost=$${result.costUsd.toFixed(2)}` : ""}`);
      if (!result.ok) throw new Error(result.summary);

      if (await git.hasUncommittedChanges(worktree)) await git.commitAll(worktree, `${task.id}: ${task.title}`);

      let prUrl: string | undefined;
      const changed = (await git.commitsAhead(repo, worktree)) > 0;
      if (changed) {
        prUrl = await git.publish(repo, worktree, branch, {
          title: `${task.id}: ${task.title}`,
          body: `${task.closingRef ?? task.url}\n\n${result.summary}\n\n🦈 Generated by Mako with ${executor.label}`,
          shortBody: task.closingRef ?? task.url,
        });
      }

      const outcome = prUrl
        ? `Pull request: ${prUrl}`
        : changed
          ? `Changes pushed to branch ${branch}.`
          : `No code changes were committed.`;
      await tracker.comment(task, `🦈 Mako: ${executor.label} finished.\n\n${outcome}\n\n${result.summary}`);
      await tracker.onFinish(task, changed ? "success" : "no-changes");
      this.state.update(task.id, {
        status: "done",
        finishedAt: new Date().toISOString(),
        prUrl,
        sessionId: result.sessionId,
        costUsd: result.costUsd,
      });
      log(task.id, `done: ${outcome}`);
      inform(`✅ ${task.id} готово`, changed ? outcome : "Агент не вніс змін, див. коментар у задачі");
    } catch (err: any) {
      if (this.stopped) {
        // Interrupted by shutdown, not a real failure: run it again after restart.
        this.state.update(task.id, { status: "queued", queuedAt: 0 });
        return;
      }
      log(task.id, `failed: ${err.message}`);
      this.state.update(task.id, { status: "failed", finishedAt: new Date().toISOString(), error: err.message });
      await tracker.comment(task, `🦈 Mako: ${executor.label} failed:\n\n${String(err.message).slice(0, 3000)}`).catch(() => {});
      await tracker.onFinish(task, "failed").catch(() => {});
      inform(`❌ ${task.id} не вдалося`, String(err.message).slice(0, 200));
    }
  }

  /** Lets a failed or finished task be offered again (inbox "Повторити"). */
  retry(id: string): boolean {
    const rec = this.state.get(id);
    if (!rec || !["failed", "done", "handed-off"].includes(rec.status)) return false;
    this.state.update(id, { status: "queued", queuedAt: Date.now(), error: undefined });
    this.schedule();
    return true;
  }
}
