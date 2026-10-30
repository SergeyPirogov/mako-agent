import type { Config } from "./config.js";
import type { Task, Tracker } from "./types.js";
import { StateStore, type TaskRecord } from "./state.js";
import { buildPrompt } from "./prompt.js";
import { createExecutor, type Executor } from "./executors/index.js";
import type { RepoConfig } from "./config.js";
import { existsSync, writeFileSync } from "node:fs";
import path from "node:path";
import { askToTake, inform, type Decision } from "./notify.js";
import * as git from "./git.js";
import { routeTask } from "./routing.js";
import { findReview } from "./reviews.js";
import { log } from "./log.js";

export class Orchestrator {
  private trackers: Map<string, Tracker>;
  private executors: Map<string, Executor>;
  private active = new Map<string, Promise<void>>();
  /** Unroutable tasks already reported, so the log is not repeated on every poll. */
  private unroutable = new Map<string, string>();
  private stopped = false;
  private lastHandoffCheck = 0;
  private wipFull = false;
  /** Paused from the tray: no new tasks are pulled; running ones continue. */
  paused = false;
  /** For the "next search" clock: when trackers were last searched and what came back. */
  lastSearch?: { at: number; found: number; added: number; errors: number };
  private nextTickAt?: number;
  private searching = false;
  private wake?: () => void;

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

  /** Applies a new config from the settings page. Running agents keep the settings they started with. */
  reload(cfg: Config, trackers: Tracker[]): void {
    this.cfg = cfg;
    this.trackers = new Map(trackers.map((t) => [t.id, t]));
    this.executors = new Map(Object.entries(cfg.executors).map(([name, c]) => [name, createExecutor(name, c)]));
    this.lastHandoffCheck = 0;
    this.schedule();
  }

  async start(): Promise<void> {
    log("main", `Polling ${[...this.trackers.values()].map((t) => t.id).join(", ")} every ${this.cfg.pollIntervalSec}s`);
    this.schedule(); // tasks queued before a restart
    while (!this.stopped) {
      await this.poll();
      const ms = this.cfg.pollIntervalSec * 1000;
      this.nextTickAt = Date.now() + ms;
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, ms);
        this.wake = () => {
          clearTimeout(timer);
          resolve();
        };
      });
      this.wake = undefined;
    }
  }

  /** Where a task may go when the developer takes it: the project's repos and every executor. */
  choicesFor(id: string): { repos: string[]; repo?: string; executors: { name: string; label: string }[]; executor: string } | undefined {
    const rec = this.state.get(id);
    const project = rec ? this.trackers.get(rec.task.trackerId)?.projects[rec.task.project] : undefined;
    if (!rec || !project) return undefined;
    const repoName = rec.chosenRepo ?? rec.repo;
    const repo = repoName ? project.repos[repoName] : undefined;
    return {
      repos: Object.keys(project.repos),
      repo: repoName,
      executors: [...this.executors.values()].map((e) => ({ name: e.name, label: e.label })),
      executor: rec.chosenExecutor ?? (repo ? this.executorFor(repo).name : this.cfg.defaultExecutor),
    };
  }

  /** Where a task's work lives, for the changes view. */
  workspaceFor(id: string): { worktree: string; baseBranch: string; task: TaskRecord["task"] } | undefined {
    const rec = this.state.get(id);
    if (!rec) return undefined;
    const repo = rec.repo ? this.trackers.get(rec.task.trackerId)?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    // While the agent works the worktree is not recorded yet, but its path is fixed.
    const worktree = rec.worktree ?? (repo ? path.join(repo.worktreesDir, rec.task.slug) : undefined);
    if (!worktree || !existsSync(worktree)) return undefined;
    return { worktree, baseBranch: repo?.baseBranch ?? "main", task: rec.task };
  }

  /** Status or label changes in the tracker must not fail the agent's work: report them and carry on. */
  private async updateTracker(task: Task, change: () => Promise<void>): Promise<void> {
    try {
      await change();
    } catch (err: any) {
      log(task.id, `tracker status not updated: ${err.message}`);
      inform(`⚠ ${task.id}: статус у трекері не змінено`, String(err.message).slice(0, 200));
    }
  }

  /** "Перевірити зараз": search the trackers without waiting for the next tick. */
  searchNow(): void {
    this.wake?.();
  }

  /** What the clock in the UI shows: when Mako looks for tasks next, or why it does not. */
  searchStatus():
    | { state: "searching" | "paused"; last?: Orchestrator["lastSearch"] }
    | { state: "wip-full"; wip: number; limit: number; last?: Orchestrator["lastSearch"] }
    | { state: "scheduled"; nextAt?: number; last?: Orchestrator["lastSearch"] } {
    const last = this.lastSearch;
    if (this.searching) return { state: "searching", last };
    if (this.paused) return { state: "paused", last };
    const limit = this.cfg.wipLimit;
    if (limit && this.wip() >= limit) return { state: "wip-full", wip: this.wip(), limit, last };
    return { state: "scheduled", nextAt: this.nextTickAt, last };
  }

  stop(): void {
    this.stopped = true;
  }

  async drain(): Promise<void> {
    await Promise.allSettled(this.active.values());
  }

  /** Tasks taken and not finished: the Kanban "in progress" column that `wipLimit` caps. */
  wip(): number {
    return this.state.all().filter((r) => r.status === "queued" || r.status === "running" || r.status === "handed-off").length;
  }

  /** How many more tasks may be offered now; a task whose notification is on screen holds a slot. */
  private freeSlots(): number {
    const limit = this.cfg.wipLimit;
    if (!limit) return Infinity;
    const asking = this.state.all().filter((r) => r.status === "asking").length;
    return limit - this.wip() - asking;
  }

  async poll(): Promise<void> {
    if (this.paused && !this.cfg.dryRun) {
      this.schedule();
      await this.maybeCheckHandoffs();
      return;
    }
    const limit = this.cfg.wipLimit;
    if (limit && !this.cfg.dryRun && this.wip() >= limit) {
      if (!this.wipFull) log("main", `WIP limit reached (${this.wip()}/${limit}): not polling trackers until a task is finished`);
      this.wipFull = true;
      this.schedule();
      await this.maybeCheckHandoffs();
      return;
    }
    if (this.wipFull) log("main", `WIP ${this.wip()}/${limit}: a slot is free, polling trackers again`);
    this.wipFull = false;

    this.searching = true;
    const search = { at: Date.now(), found: 0, added: 0, errors: 0 };
    const known = new Set(this.state.all().map((r) => r.id));
    try {
      for (const tracker of this.trackers.values()) {
        let tasks: Task[];
        try {
          tasks = await tracker.fetchTasks();
          search.found += tasks.length;
        } catch (err: any) {
          search.errors++;
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
            log(task.id, `[dry-run] → ${route.repo.name} (${route.repo.repoPath}) by ${route.reason}, executor ${ex.name}; prompt:\n${buildPrompt(task, tracker, route.repo, ex)}\n`);
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
    } finally {
      search.added = this.state.all().filter((r) => !known.has(r.id)).length;
      this.lastSearch = search;
      this.searching = false;
    }
    this.schedule();
    await this.maybeCheckHandoffs();
  }

  private async maybeCheckHandoffs(): Promise<void> {
    const every = this.cfg.handoffCheckMin * 60_000;
    if (every > 0 && Date.now() - this.lastHandoffCheck >= every) {
      this.lastHandoffCheck = Date.now();
      await this.checkHandoffs();
    }
  }

  /** A handed-off task is finished from Mako's side once a PR/MR is opened from its branch. */
  async checkHandoffs(): Promise<void> {
    for (const rec of this.state.all().filter((r) => r.status === "handed-off")) {
      const tracker = this.trackers.get(rec.task.trackerId);
      const repo = rec.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
      if (!tracker || !repo) continue;
      const branch = rec.branch ?? git.branchFor(rec.task.slug);
      try {
        const { review, branchSha } = await findReview(repo, branch, rec.branchSha);
        if (branchSha && branchSha !== rec.branchSha) this.state.update(rec.id, { branchSha });
        if (!review) {
          // The tool's workspace is gone and nothing was opened from it: the hand-off was abandoned.
          if (rec.worktree && !existsSync(rec.worktree)) {
            this.cancel(rec.id, `Workspace видалено (${rec.worktree}), PR не відкрито`);
            inform(`🦈 ${rec.id}: передачу скасовано`, "Workspace видалено, PR немає. Задачу можна повторити з дошки.");
          }
          continue;
        }

        const name = review.kind === "pull" ? `Pull request #${review.number}` : `Merge request !${review.number}`;
        const link = review.url ?? name;
        log(rec.id, `review found: ${link}`);
        await tracker.comment(rec.task, `🦈 Mako: ${name} opened for this issue: ${link}`);
        await this.updateTracker(rec.task, () => tracker.onFinish(rec.task, "success"));
        this.state.update(rec.id, { status: "done", prUrl: review.url, finishedAt: new Date().toISOString() });
        inform(`✅ ${rec.id}: є ${review.kind === "pull" ? "PR" : "MR"}`, link);
      } catch (err: any) {
        log(rec.id, `review check failed: ${err.message}`);
      }
    }
  }

  private onTaskSeen(task: Task, tracker: Tracker, repo: string): void {
    const rec = this.state.get(task.id);
    // Pull only as much work as the WIP limit allows; the rest stays in the tracker for later polls.
    // With notifications off, every task waits in the inbox and the developer picks.
    const offers = this.cfg.notifications.enabled || this.cfg.notifications.autoTake;
    if (!rec && offers && this.freeSlots() <= 0) return;
    if (!rec) {
      log(task.id, `new task → ${repo}: ${task.title}`);
      this.state.update(task.id, { task, repo, status: "snoozed", snoozedUntil: 0 });
    } else if (rec.status === "snoozed" || rec.status === "queued") {
      // Keep the freshest description, comments and routing, unless the developer chose the repo.
      this.state.update(task.id, rec.chosenRepo ? { task } : { task, repo });
    }
    const current = this.state.get(task.id)!;
    const due = current.status === "snoozed" && (current.snoozedUntil ?? 0) <= Date.now();
    if (due && this.freeSlots() > 0) this.ask(current, tracker);
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
  decide(id: string, decision: Decision, choice: { repo?: string; executor?: string } = {}): boolean {
    const rec = this.state.get(id);
    if (!rec || !["asking", "snoozed"].includes(rec.status)) return false;
    if (decision === "take") {
      const project = this.trackers.get(rec.task.trackerId)?.projects[rec.task.project];
      const chosenRepo = choice.repo && project?.repos[choice.repo] ? choice.repo : undefined;
      const chosenExecutor = choice.executor && this.executors.has(choice.executor) ? choice.executor : undefined;
      log(id, `taken${chosenRepo ? ` → repo ${chosenRepo}` : ""}${chosenExecutor ? ` · executor ${chosenExecutor}` : ""}`);
      this.state.update(id, {
        status: "queued",
        queuedAt: Date.now(),
        chosenRepo,
        chosenExecutor,
        ...(chosenRepo ? { repo: chosenRepo } : {}),
      });
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
    const repo =
      (rec.chosenRepo && project?.repos[rec.chosenRepo]) || (rec.repo && project?.repos[rec.repo]) || routeTask(task, project).repo;
    if (!tracker || !repo) {
      this.state.update(task.id, { status: "failed", error: `no configured repository for ${task.id}` });
      return;
    }
    const executor = (rec.chosenExecutor && this.executors.get(rec.chosenExecutor)) || this.executorFor(repo);
    const branch = git.branchFor(task.slug);
    const prompt = buildPrompt(task, tracker, repo, executor);
    const promptFile = this.state.logPath(task).replace(/\.jsonl$/, ".prompt.md");
    writeFileSync(promptFile, prompt);
    this.state.update(task.id, {
      status: "running",
      startedAt: new Date().toISOString(),
      repo: repo.name,
      executor: executor.name,
    });

    try {
      await this.updateTracker(task, () => tracker.onStart(task));
      log(task.id, `starting ${executor.name} (${executor.label}) for repo ${repo.name}`);
      const result = await executor.run(
        {
          task,
          tracker,
          repo,
          branch,
          logPath: this.state.logPath(task),
          promptFile,
          timeoutMin: this.cfg.taskTimeoutMin,
          ui: this.cfg.notifications.inboxPort
            ? { port: this.cfg.notifications.inboxPort, remindMin: this.cfg.notifications.remindMin }
            : undefined,
        },
        prompt,
      );

      if (result.kind === "handed-off") {
        const where = result.details ? ` (${result.details})` : "";
        await tracker.comment(task, `🦈 Mako handed this issue to ${executor.label}${where} on branch ${result.branch ?? branch}.`);
        this.state.update(task.id, {
          status: "handed-off",
          worktree: result.worktree,
          branch: result.branch ?? branch,
        });
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
      await this.updateTracker(task, () => tracker.onFinish(task, changed ? "success" : "no-changes"));
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
  retry(id: string, choice: { repo?: string; executor?: string } = {}): boolean {
    const rec = this.state.get(id);
    if (!rec || !["failed", "done", "handed-off", "cancelled"].includes(rec.status)) return false;
    const project = this.trackers.get(rec.task.trackerId)?.projects[rec.task.project];
    const chosenRepo = choice.repo && project?.repos[choice.repo] ? choice.repo : rec.chosenRepo;
    const chosenExecutor = choice.executor && this.executors.has(choice.executor) ? choice.executor : rec.chosenExecutor;
    log(id, `retry${chosenRepo ? ` → repo ${chosenRepo}` : ""}${chosenExecutor ? ` · executor ${chosenExecutor}` : ""}`);
    this.state.update(id, {
      status: "queued",
      queuedAt: Date.now(),
      error: undefined,
      chosenRepo,
      chosenExecutor,
      ...(chosenRepo ? { repo: chosenRepo } : {}),
      // A fresh run, not the old hand-off: forget its branch and PR tracking.
      branch: undefined,
      branchSha: undefined,
    });
    this.schedule();
    return true;
  }

  /** Stops waiting for a task that is queued or handed off (e.g. its Orca workspace was deleted); frees its WIP slot. */
  cancel(id: string, reason = "Скасовано вручну"): boolean {
    const rec = this.state.get(id);
    if (!rec || !["queued", "handed-off"].includes(rec.status)) return false;
    log(id, `cancelled: ${reason}`);
    this.state.update(id, { status: "cancelled", finishedAt: new Date().toISOString(), error: reason });
    this.schedule();
    return true;
  }
}
