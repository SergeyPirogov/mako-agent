import type { Config } from "./config.js";
import type { IssueState, Outcome, Task, Tracker } from "./types.js";
import { StateStore, type TaskRecord } from "./state.js";
import { buildPrompt } from "./prompt.js";
import { createExecutor, type Executor } from "./executors/index.js";
import type { RepoConfig } from "./config.js";
import { existsSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { askToTake, inform, type Decision } from "./notify.js";
import * as git from "./git.js";
import { routeTask } from "./routing.js";
import { findReview } from "./reviews.js";
import { log, keepLogsIn, dropLog } from "./log.js";
import { sessions, sessionIdFor } from "./sessions.js";
import { stopTask } from "./executors/processes.js";
import { commitMessage, reviewText, taskSummary } from "./commit-message.js";

export class Orchestrator {
  /** By id; replaced on reload. */
  trackers: Map<string, Tracker>;
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
    keepLogsIn(path.join(cfg.stateDir, "activity"));
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
    this.reviewTimer = setInterval(() => void this.refreshReviews(), 60_000);
    void this.refreshReviews();
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
  /** Status changes never stop the work; the card says whether the issue moved, and why not. */
  private async updateTracker(task: Task, moment: "start" | Outcome | "merged", change: () => Promise<string | void>): Promise<void> {
    const tracker = this.trackers.get(task.trackerId);
    try {
      const name = await change();
      if (name) {
        log(task.id, `tracker status → ${name}`);
        this.state.update(task.id, { trackerStatus: { ok: true, text: `Статус у ${tracker!.name}: «${name}»` } });
      } else if (tracker?.statusFor && moment === "start") {
        this.state.update(task.id, { trackerStatus: { ok: false, text: `Статус у ${tracker.name} не змінюється: у Налаштуваннях не вибрано, на який` } });
      }
    } catch (err: any) {
      log(task.id, `tracker status not updated: ${err.message}`);
      this.state.update(task.id, { trackerStatus: { ok: false, text: `Статус у ${tracker?.name ?? "трекері"} не змінено: ${err.message}` } });
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

  private reviewTimer?: NodeJS.Timeout;

  stop(): void {
    this.stopped = true;
    clearInterval(this.reviewTimer);
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
        // With colleagues on Mako too: a task already assigned to someone else is theirs.
        const me = tracker.whoAmI ? (await tracker.whoAmI().catch(() => undefined))?.toLowerCase() : undefined;
        for (const task of tasks) {
          seen.add(task.id);
          const owners = (task.assignees ?? []).map((a) => a.toLowerCase());
          if (me && owners.length && !owners.includes(me)) {
            const rec = this.state.get(task.id);
            if (rec && (rec.status === "asking" || rec.status === "snoozed")) {
              log(task.id, `assigned to ${task.assignees!.join(", ")}: no longer offered`);
              this.state.delete(task.id);
            }
            continue;
          }
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
        // A task waiting for an answer that left the query: deleted or closed ones are marked on the board,
        // the rest (reassigned, label removed) no longer need an answer and are forgotten.
        for (const rec of this.state.all()) {
          if (rec.task.trackerId !== tracker.id || seen.has(rec.id) || !["asking", "snoozed"].includes(rec.status)) continue;
          const state = await this.issueState(tracker, rec);
          if (state === "closed" || state === "deleted") this.markGone(rec, state);
          else if (state === "open") {
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
      await this.checkGone();
    }
  }

  /** PRs/MRs Mako opened: once merged, the issue gets its final status and the card says so. */
  async checkMerged(): Promise<void> {
    await this.refreshReviews();
  }

  /** PRs/MRs Mako opened and still watches: open, not archived, recent. */
  private watchedReviews(): TaskRecord[] {
    const recent = Date.now() - 30 * 24 * 3600_000;
    return this.state
      .all()
      .filter((r) => r.prUrl && !r.reviewState && !r.archivedAt && ["queued", "running", "done", "handed-off"].includes(r.status))
      .filter((r) => !r.finishedAt || Date.parse(r.finishedAt) > recent);
  }

  private reviewsBusy = false;
  /** When each PR/MR was last looked at (not saved: it changes every minute). */
  readonly reviewChecked = new Map<string, number>();

  /** CI, review threads, conflicts and merges of the watched PRs/MRs; runs every minute. */
  async refreshReviews(): Promise<void> {
    if (this.reviewsBusy) return;
    this.reviewsBusy = true;
    try {
      for (const rec of this.watchedReviews()) await this.refreshReview(rec.id);
    } finally {
      this.reviewsBusy = false;
    }
  }

  async refreshReview(id: string): Promise<TaskRecord | undefined> {
    const rec = this.state.get(id);
    const tracker = rec && this.trackers.get(rec.task.trackerId);
    if (!rec?.prUrl || !tracker) return rec;
    try {
      if (tracker.reviewDetails) {
        const review = await tracker.reviewDetails(rec.prUrl);
        const before = rec.review;
        this.reviewChecked.set(id, Date.now());
        // Only real changes are saved: each save redraws the board.
        const same = before && JSON.stringify({ ...before, checkedAt: 0 }) === JSON.stringify({ ...review, checkedAt: 0 });
        if (!same) this.state.update(id, { review });
        if (review.pipeline?.status === "failed" && before?.pipeline?.status !== "failed") {
          inform(`❌ ${id}: CI впав`, `${review.pipeline.failed.map((j) => j.name).join(", ") || "пайплайн"} · можна віддати агенту`);
        }
        if (review.conflicts && !before?.conflicts) inform(`⚠ ${id}: конфлікти з основною гілкою`, rec.task.title);
        if (review.comments.length > (before?.comments.length ?? 0)) {
          inform(`💬 ${id}: нові коментарі в ${this.reviewKind(rec)}`, review.comments.at(-1)!.body.slice(0, 160));
        }
        if (review.state !== "open") await this.reviewEnded(this.state.get(id)!, review.state, tracker);
      } else if (tracker.reviewState) {
        const state = await tracker.reviewState(rec.prUrl);
        if (state !== "open") await this.reviewEnded(rec, state, tracker);
      }
    } catch (err: any) {
      log(id, `review check failed: ${err.message}`);
    }
    return this.state.get(id);
  }

  private reviewKind(rec: TaskRecord): string {
    return rec.prUrl?.includes("/merge_requests/") ? "MR" : "PR";
  }

  private async reviewEnded(rec: TaskRecord, state: "merged" | "closed", tracker: Tracker): Promise<void> {
    if (rec.reviewState) return;
    this.state.update(rec.id, { reviewState: state });
    if (state === "closed") return log(rec.id, "review closed without merging");
    log(rec.id, `review merged: ${rec.prUrl}`);
    if (tracker.onMerged) await this.updateTracker(rec.task, "merged", () => tracker.onMerged!(rec.task));
    inform(`✅ ${rec.id}: ${this.reviewKind(rec)} змерджено`, rec.status === "running" ? "Агент ще в терміналі: натисніть «Завершити»" : rec.task.title);
  }

  /** Merges the PR/MR from Mako; the issue then moves on as for a merge done in GitLab. */
  async mergeReview(id: string): Promise<void> {
    const rec = this.state.get(id);
    const tracker = rec && this.trackers.get(rec.task.trackerId);
    if (!rec?.prUrl || !tracker?.mergeReview) throw new Error("Немає MR, який Mako може змерджити");
    await tracker.mergeReview(rec.prUrl);
    log(id, `merged from Mako: ${rec.prUrl}`);
    await this.refreshReview(id);
  }

  /**
   * Hands the agent what the PR/MR needs: the failed CI jobs with their logs, the open review threads,
   * or the conflicts. A live terminal gets it as a message; otherwise a new run starts in the same worktree.
   */
  async askAgent(id: string, kind: "ci" | "comments" | "conflicts"): Promise<"sent" | "started"> {
    let rec = await this.refreshReview(id);
    const tracker = rec && this.trackers.get(rec.task.trackerId);
    const repo = rec?.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    const review = rec?.review;
    if (!rec || !tracker || !review || !rec.prUrl) throw new Error("Для цієї задачі немає MR");
    const kindName = this.reviewKind(rec);
    const tail = "\n\nDo not push and do not open another merge request: Mako pushes the branch and updates the existing one.";
    let text: string;
    if (kind === "ci") {
      const failed = review.pipeline?.failed ?? [];
      if (review.pipeline?.status !== "failed") throw new Error("CI не впав");
      const logs = [];
      for (const job of failed.slice(0, 3)) {
        const log = job.id && tracker.jobLog ? await tracker.jobLog(rec.prUrl, job.id).catch(() => "") : "";
        logs.push(`### ${job.name}${job.url ? ` (${job.url})` : ""}\n${log ? "```\n" + log.trim() + "\n```" : "(log not available)"}`);
      }
      text = `The CI pipeline of ${kindName} ${rec.prUrl} failed${review.pipeline.url ? ` (${review.pipeline.url})` : ""}.\n\n${logs.join("\n\n") || "No failed job details were available."}\n\nFind the cause, fix it in this worktree, run the same checks locally where you can, and commit.${tail}`;
    } else if (kind === "comments") {
      if (!review.comments.length) throw new Error("Відкритих коментарів немає");
      const list = review.comments
        .map((c, i) => `${i + 1}. ${c.author}${c.path ? ` on ${c.path}${c.line ? `:${c.line}` : ""}` : ""}${c.url ? ` (${c.url})` : ""}:\n${c.body.trim().split("\n").map((l) => `> ${l}`).join("\n")}`)
        .join("\n\n");
      text = `Reviewers left unresolved comments on ${kindName} ${rec.prUrl}:\n\n${list}\n\nAddress each one in the code. If you disagree with a comment, do not change the code for it; say why in your final message. Commit the changes.${tail}`;
    } else {
      if (!review.conflicts) throw new Error("Конфліктів немає");
      const base = repo?.baseBranch ?? "main";
      text = `${kindName} ${rec.prUrl} has conflicts with ${base}. Run \`git fetch origin ${base}\` and \`git merge origin/${base}\`, resolve the conflicts keeping the intent of both sides, run the checks, and commit the merge. Do not rebase.${tail}`;
    }
    const session = sessions.get(sessionIdFor(rec.task));
    if (session && rec.status === "running" && session.say(text)) {
      log(id, `asked the agent: ${kind}`);
      return "sent";
    }
    // No live terminal: a new run in the same worktree, with just this request.
    this.state.update(id, { followUp: text });
    if (!this.retry(id, { executor: this.terminalExecutor() })) throw new Error("Задачу зараз не можна перезапустити");
    log(id, `follow-up run: ${kind}`);
    rec = this.state.get(id);
    return "started";
  }

  /** «Взяти» puts the issue on the developer in the tracker, so colleagues do not take it too (assignOnTake: false turns it off). */
  private async assignOnTake(id: string): Promise<void> {
    const rec = this.state.get(id);
    const tracker = rec && this.trackers.get(rec.task.trackerId);
    if (!rec || !tracker?.assignToMe) return;
    const raw = (this.cfg.trackers as any[]).find((t) => (t.id ?? t.type) === tracker.id);
    if (raw?.assignOnTake === false) return;
    const me = await tracker.whoAmI?.().catch(() => undefined);
    if (me && rec.task.assignees?.some((a) => a.toLowerCase() === me.toLowerCase())) return;
    try {
      await tracker.assignToMe(rec.task);
      log(id, `assigned to ${me ?? "me"} in ${tracker.name}`);
    } catch (err: any) {
      log(id, `not assigned in ${tracker.name}: ${err.message}`);
    }
  }

  /** Taken tasks are not in every search (the WIP limit pauses it), so their issues are looked up one by one. */
  async checkGone(): Promise<void> {
    for (const rec of this.state.all().filter((r) => ["queued", "running", "handed-off"].includes(r.status))) {
      const tracker = this.trackers.get(rec.task.trackerId);
      if (!tracker) continue;
      const state = await this.issueState(tracker, rec);
      if ((state === "closed" || state === "deleted") && rec.gone !== state) this.markGone(rec, state);
      if (state === "open" && rec.gone) this.state.update(rec.id, { gone: undefined });
    }
  }

  /** undefined when the tracker could not be asked (network, rights): better to wait than to drop a task. */
  private async issueState(tracker: Tracker, rec: TaskRecord): Promise<IssueState | undefined> {
    if (!tracker.issueState) return "open";
    try {
      return await tracker.issueState(rec.task);
    } catch (err: any) {
      log(rec.id, `issue check failed: ${err.message}`);
      return undefined;
    }
  }

  /** Not yet started: nothing to do any more. Already with an agent: the developer decides, the card says why. */
  private markGone(rec: TaskRecord, state: "closed" | "deleted"): void {
    const where = this.trackers.get(rec.task.trackerId)?.name ?? "трекері";
    const what = state === "deleted" ? `Задачу видалено в ${where}` : `Задачу закрито в ${where}`;
    log(rec.id, `${state} in the tracker`);
    if (["asking", "snoozed", "queued"].includes(rec.status)) {
      this.state.update(rec.id, { status: "cancelled", gone: state, error: what, finishedAt: new Date().toISOString() });
      this.schedule();
    } else {
      this.state.update(rec.id, { gone: state });
      inform(`🦈 ${rec.id}: ${what.toLowerCase()}`, "Агент ще працює над нею. Зупиніть або доведіть до кінця на дошці Mako.");
    }
  }

  /** A handed-off task is finished from Mako's side once a PR/MR is opened from its branch. */
  async checkHandoffs(): Promise<void> {
    for (const rec of this.state.all().filter((r) => r.status === "handed-off")) {
      const tracker = this.trackers.get(rec.task.trackerId);
      const repo = rec.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
      if (!tracker || !repo) continue;
      const branch = rec.branch ?? this.branchOf(rec);
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
        await this.updateTracker(rec.task, "success", () => tracker.onFinish(rec.task, "success"));
        this.state.update(rec.id, { status: "done", prUrl: review.url, finishedAt: new Date().toISOString() });
        inform(`✅ ${rec.id}: є ${review.kind === "pull" ? "PR" : "MR"}`, link);
      } catch (err: any) {
        log(rec.id, `review check failed: ${err.message}`);
      }
    }
  }

  private onTaskSeen(task: Task, tracker: Tracker, repo: string): void {
    const rec = this.state.get(task.id);
    if (rec?.gone && rec.status === "cancelled") {
      // Reopened or restored in the tracker: offer it again.
      log(task.id, "is back in the tracker");
      this.state.update(task.id, { task, status: "snoozed", snoozedUntil: 0, gone: undefined, error: undefined, archivedAt: undefined });
    }
    // Pull only as much work as the WIP limit allows; the rest stays in the tracker for later polls.
    // With notifications off, every task waits in the inbox and the developer picks.
    const offers = this.cfg.notifications.enabled || this.cfg.notifications.autoTake;
    if (!rec && offers && this.freeSlots() <= 0) return;
    if (!rec) {
      log(task.id, `new task → ${repo}: ${task.title}`);
      this.state.update(task.id, { task, repo, status: "snoozed", snoozedUntil: 0 });
    } else if (rec.status === "snoozed" || rec.status === "queued") {
      // Keep the freshest description, comments and routing, unless the developer chose the repo.
      // A fresh attempt has its own slug (worktree, branch): keep it over the tracker's.
      const fresh = { ...task, slug: rec.task.slug };
      this.state.update(task.id, rec.chosenRepo ? { task: fresh } : { task: fresh, repo });
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
      void this.assignOnTake(id);
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
    // Named once, from the pattern: later runs and the MR use the same branch.
    // An existing worktree (a task from before patterns, or Orca's) keeps the branch it already has.
    const branch =
      rec.branchName ??
      (await git.currentBranch(path.join(repo.worktreesDir, task.slug))) ??
      git.branchName(repo.branchPattern, task, rec.attempt);
    // A follow-up (fix CI, review comments, conflicts) continues the work in the same worktree.
    const prompt = rec.followUp
      ? `You are continuing work on ${task.id}: ${task.title} (${task.url}) in this worktree, on branch ${branch}.\n\n${rec.followUp}`
      : buildPrompt(task, tracker, repo, executor);
    const promptFile = this.state.logPath(task).replace(/\.jsonl$/, ".prompt.md");
    writeFileSync(promptFile, prompt);
    this.state.update(task.id, {
      status: "running",
      startedAt: new Date().toISOString(),
      repo: repo.name,
      executor: executor.name,
      resumeSession: undefined,
      followUp: undefined,
      branchName: branch,
    });

    try {
      await this.updateTracker(task, "start", () => tracker.onStart(task));
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
          resumeSessionId: rec.resumeSession,
          onSession: (sessionId) => this.state.update(task.id, { sessionId }),
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

      if (this.takeOvers.delete(task.id)) return this.continueInTerminal(task.id);
      const { worktree } = result;
      this.state.update(task.id, { worktree, ...(result.sessionId ? { sessionId: result.sessionId } : {}) });
      log(task.id, `agent finished: ok=${result.ok}${result.costUsd !== undefined ? `, cost=$${result.costUsd.toFixed(2)}` : ""}`);
      if (!result.ok) throw new Error(result.summary);

      await this.commitLeftovers(worktree, task);

      let prUrl: string | undefined = this.state.get(task.id)?.prUrl;
      const changed = (await git.commitsAhead(repo, worktree)) > 0;
      if (result.denied?.length && !changed) {
        // It wanted to do more than it was allowed to: that is not "done", the developer has to step in.
        const denied = [...new Set(result.denied)].slice(0, 8);
        this.state.update(task.id, { denied });
        const list = denied.map((d) => `- ${d}`).join("\n");
        throw new Error(
          `Агенту бракувало дозволів, тому він не завершив задачу:\n${list}\n\n` +
            "«Продовжити в терміналі» відкриє ту саму розмову: підтвердіть дозволи, і агент продовжить. " +
            "Або додайте ці команди в --allowedTools виконавця в Налаштуваннях.",
        );
      }
      // «Створити MR» already pushed this exact commit: the MR may be merged by now, pushing again would reopen it.
      const pushed = this.state.get(task.id)?.publishedSha;
      const announced = this.state.get(task.id)?.reviewAnnounced;
      let pushedNow = false;
      if (changed && !(pushed && pushed === (await git.headSha(worktree)))) {
        prUrl = (await this.openReview(task, tracker, repo, worktree, branch, executor.label)).url ?? prUrl;
        pushedNow = true;
      }

      const outcome = prUrl
        ? `Pull request: ${prUrl}`
        : changed
          ? `Changes pushed to branch ${branch}.`
          : `No code changes were committed.`;
      // The MR was announced in the issue when it was opened: say more only if finishing pushed new work.
      if (!announced) await tracker.comment(task, `🦈 Mako: ${executor.label} finished.\n\n${outcome}\n\n${result.summary}`);
      else if (pushedNow) await tracker.comment(task, `🦈 Mako: ${executor.label} finished and pushed the last changes to ${prUrl}\n\n${result.summary}`);
      await this.updateTracker(task, changed ? "success" : "no-changes", () => tracker.onFinish(task, changed ? "success" : "no-changes"));
      const summary = await taskSummary(worktree, repo.baseBranch, task, result.summary, prUrl).catch((err: any) => ({ text: result.summary, error: err.message }));
      if (summary.error) log(task.id, `summary not written by Claude: ${summary.error}`);
      this.state.update(task.id, {
        status: "done",
        finishedAt: new Date().toISOString(),
        prUrl,
        sessionId: result.sessionId,
        costUsd: result.costUsd,
        summary: summary.text || undefined,
      });
      log(task.id, `done: ${outcome}`);
      inform(`✅ ${task.id} готово`, changed ? outcome : "Агент не вніс змін, див. коментар у задачі");
    } catch (err: any) {
      if (this.takeOvers.delete(task.id)) return this.continueInTerminal(task.id);
      if (this.stopped) {
        // Interrupted by shutdown, not a real failure: run it again after restart.
        this.state.update(task.id, { status: "queued", queuedAt: 0 });
        return;
      }
      log(task.id, `failed: ${err.message}`);
      this.state.update(task.id, { status: "failed", finishedAt: new Date().toISOString(), error: err.message });
      await tracker.comment(task, `🦈 Mako: ${executor.label} failed:\n\n${String(err.message).slice(0, 3000)}`).catch(() => {});
      await this.updateTracker(task, "failed", () => tracker.onFinish(task, "failed"));
      inform(`❌ ${task.id} не вдалося`, String(err.message).slice(0, 200));
    }
  }

  /** Lets a failed or finished task be offered again (inbox "Повторити"). */
  retry(id: string, choice: { repo?: string; executor?: string; resume?: boolean; fresh?: boolean } = {}): boolean {
    const rec = this.state.get(id);
    if (!rec || !["failed", "done", "handed-off", "cancelled"].includes(rec.status)) return false;
    const project = this.trackers.get(rec.task.trackerId)?.projects[rec.task.project];
    const chosenRepo = choice.repo && project?.repos[choice.repo] ? choice.repo : rec.chosenRepo;
    const resume = choice.resume && rec.sessionId ? this.terminalExecutor() : undefined;
    const chosenExecutor = resume ?? (choice.executor && this.executors.has(choice.executor) ? choice.executor : rec.chosenExecutor);
    // From scratch: a new worktree and branch from the latest base; the previous attempt (and its MR) stays as it is.
    const attempt = choice.fresh && !resume ? (rec.attempt ?? 1) + 1 : rec.attempt;
    const slug = choice.fresh && !resume ? `${rec.task.slug.replace(/-try\d+$/, "")}-try${attempt}` : rec.task.slug;
    if (choice.fresh && !resume) {
      this.state.update(id, {
        task: { ...rec.task, slug },
        attempt,
        worktree: undefined,
        prUrl: undefined,
        publishedSha: undefined,
        reviewAnnounced: undefined,
        branchName: undefined,
        sessionId: undefined,
        trackerStatus: undefined,
      });
    }
    log(id, `${resume ? "continue in terminal" : choice.fresh ? `start over in a new worktree (${slug})` : "retry"}${chosenRepo ? ` → repo ${chosenRepo}` : ""}${chosenExecutor ? ` · executor ${chosenExecutor}` : ""}`);
    this.state.update(id, {
      status: "queued",
      queuedAt: Date.now(),
      error: undefined,
      chosenRepo,
      chosenExecutor,
      resumeSession: resume ? rec.sessionId : undefined,
      denied: undefined,
      ...(chosenRepo ? { repo: chosenRepo } : {}),
      // A fresh run, not the old hand-off: forget its branch and PR tracking.
      branch: undefined,
      branchSha: undefined,
    });
    this.schedule();
    return true;
  }

  /** Uncommitted work gets a commit message written from its diff. */
  private async commitLeftovers(worktree: string, task: Task): Promise<{ text: string; error?: string } | undefined> {
    if (!(await git.hasUncommittedChanges(worktree))) return undefined;
    await git.stageAll(worktree);
    const message = await commitMessage(worktree, task);
    if (message.error) log(task.id, `commit message not written by Claude: ${message.error}`);
    await git.commitAll(worktree, message.text);
    log(task.id, `committed: ${message.text.split("\n")[0]}`);
    return message;
  }

  /**
   * Pushes the branch and opens the PR/MR, then gives it a title and description written from the branch's
   * changes, the way Mako writes its comments in the issue.
   */
  private async openReview(
    task: Task,
    tracker: Tracker,
    repo: RepoConfig,
    worktree: string,
    branch: string,
    agent: string,
  ): Promise<{ url?: string; title: string; summary: string; error?: string }> {
    const text = await reviewText(worktree, repo.baseBranch, task);
    if (text.error) log(task.id, `review text not written by Claude: ${text.error}`);
    const closing = task.closingRef ?? task.url;
    const description = [
      text.summary,
      `${closing}`,
      `🦈 Mako · ${agent} · [${task.id}](${task.url})`,
    ].filter(Boolean).join("\n\n");
    const url = await git.publish(repo, worktree, branch, { title: text.title, body: description, shortBody: closing }, hostOf(tracker));
    let error = text.error;
    if (url && tracker.describeReview) {
      // Push options take one line only: the full description goes through the tracker's API.
      await tracker.describeReview(url, text.title, description).catch((err: any) => {
        log(task.id, `review description not set: ${err.message}`);
        error = `опис не записано: ${err.message}`;
      });
    }
    return { url, title: text.title, summary: text.summary, error };
  }

  /**
   * «Створити MR» on a running session: commit what is there, push the branch and open (or update) the PR/MR,
   * while the agent keeps going. Finishing later does not open another one.
   */
  async publishNow(id: string): Promise<{ url?: string; message?: string; title: string; updated: boolean; warning?: string }> {
    const rec = this.state.get(id);
    const ws = this.workspaceFor(id);
    const tracker = rec && this.trackers.get(rec.task.trackerId);
    const repo = rec?.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    if (!rec || !ws || !tracker || !repo) throw new Error("Для цієї задачі ще немає робочої копії");
    const { task } = rec;
    const message = await this.commitLeftovers(ws.worktree, task);
    if ((await git.commitsAhead(repo, ws.worktree)) === 0) throw new Error("Змін для MR ще немає: агент нічого не змінив");
    const first = !rec.prUrl;
    const branch = this.branchOf(rec);
    const agent = (rec.executor && this.executors.get(rec.executor)?.label) ?? "агент";
    const review = await this.openReview(task, tracker, repo, ws.worktree, branch, agent);
    const url = review.url ?? rec.prUrl;
    this.state.update(id, { prUrl: url, publishedSha: await git.headSha(ws.worktree) });
    log(id, `${first ? "review opened" : "review updated"}${url ? `: ${url}` : ` (branch ${branch} pushed)`}`);
    if (first && url) {
      // The same kind of report the issue gets when a task is finished: what the MR changes and where it is.
      const kind = tracker.name === "GitHub" ? "pull request" : "merge request";
      const report = [`🦈 Mako opened a ${kind}: ${url}`, `**${review.title}**`, review.summary].filter(Boolean).join("\n\n");
      await tracker
        .comment(task, report)
        .then(() => this.state.update(id, { reviewAnnounced: true }))
        .catch((err: any) => log(id, `comment not posted: ${err.message}`));
      await this.updateTracker(task, "success", () => tracker.onFinish(task, "success"));
    }
    const problems = [message?.error && message.error !== "немає змін" ? `коміт: ${message.error}` : "", review.error ? `MR: ${review.error}` : ""].filter(Boolean);
    return { url, message: message?.text, title: review.title, updated: !first, warning: problems.join("; ") || undefined };
  }

  /**
   * Whether «Створити/Оновити MR» has anything to do: uncommitted work, or commits that are not in the
   * PR/MR yet (since its last push), or, before there is one, any change against the base.
   */
  async pendingChanges(id: string): Promise<{ hasChanges: boolean; pending: boolean }> {
    const rec = this.state.get(id);
    const ws = this.workspaceFor(id);
    const repo = rec?.repo ? this.trackers.get(rec.task.trackerId)?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    if (!rec || !ws || !repo) return { hasChanges: false, pending: false };
    try {
      const dirty = await git.hasUncommittedChanges(ws.worktree);
      const ahead = (await git.commitsAhead(repo, ws.worktree)) > 0;
      const hasChanges = dirty || ahead;
      const pending = dirty || (rec.publishedSha ? (await git.headSha(ws.worktree)) !== rec.publishedSha : ahead);
      return { hasChanges, pending };
    } catch {
      return { hasChanges: false, pending: false };
    }
  }

  /** The branch a task runs on: named from the pattern at its first run; tasks from before patterns keep agent/<slug>. */
  private branchOf(rec: TaskRecord): string {
    return rec.branchName ?? git.branchFor(rec.task.slug);
  }

  /** Tasks whose background agent is being stopped to continue in the terminal. */
  private takeOvers = new Set<string>();

  /**
   * Stops a background Claude run and opens the same conversation in Mako's terminal, so the developer
   * sees what happens and can grant what it is missing.
   */
  takeOver(id: string): boolean {
    const rec = this.state.get(id);
    if (!rec || rec.status !== "running" || !rec.sessionId || !this.terminalExecutor()) return false;
    this.takeOvers.add(id);
    if (!stopTask(id)) {
      this.takeOvers.delete(id);
      return false;
    }
    log(id, "stopping the background agent to continue in the terminal");
    return true;
  }

  private continueInTerminal(id: string): void {
    const rec = this.state.get(id)!;
    log(id, "continue in terminal");
    this.state.update(id, {
      status: "queued",
      queuedAt: Date.now(),
      error: undefined,
      chosenExecutor: this.terminalExecutor(),
      resumeSession: rec.sessionId,
    });
    this.schedule();
  }

  /** A Claude Code terminal executor, which can continue a background conversation with the developer present. */
  terminalExecutor(): string | undefined {
    return Object.entries(this.cfg.executors).find(
      ([, c]: [string, any]) => c.type === "terminal" && /^claude(\.cmd|\.exe)?$/i.test((c.command ?? "claude").split(/[\\/]/).pop()),
    )?.[0];
  }

  /** Moves finished tasks (done, failed, cancelled) off the board into the archive. */
  archive(ids: string[]): number {
    let n = 0;
    for (const id of ids) {
      const rec = this.state.get(id);
      if (!rec || rec.archivedAt || !["done", "failed", "cancelled"].includes(rec.status)) continue;
      this.state.update(id, { archivedAt: Date.now() });
      n++;
    }
    if (n) log("main", `archived ${n} task(s)`);
    return n;
  }

  unarchive(id: string): boolean {
    const rec = this.state.get(id);
    if (!rec?.archivedAt) return false;
    this.state.update(id, { archivedAt: undefined });
    return true;
  }

  /**
   * Forgets archived tasks for good, with their worktree, local branch and logs; the issue, the pushed branch and
   * the PR/MR stay. A task still matching the tracker query will be offered again.
   */
  async remove(ids: string[]): Promise<number> {
    let n = 0;
    for (const id of ids) {
      const rec = this.state.get(id);
      if (!rec?.archivedAt) continue;
      const repo = rec.repo ? this.trackers.get(rec.task.trackerId)?.projects[rec.task.project]?.repos[rec.repo] : undefined;
      const dir = rec.worktree ?? (repo ? path.join(repo.worktreesDir, rec.task.slug) : undefined);
      if (repo && dir) {
        const removed = await git.removeWorktree(repo, dir, rec.branch ?? this.branchOf(rec)).catch((err: any) => {
          log(id, `worktree not removed: ${err.message}`);
          return false;
        });
        if (removed) log(id, `worktree removed: ${dir}`);
      }
      const logFile = this.state.logPath(rec.task);
      for (const f of [logFile, logFile.replace(/\.jsonl$/, ".prompt.md"), logFile.replace(/\.jsonl$/, ".terminal.log"), logFile.replace(/\.jsonl$/, ".claude-settings.json")]) {
        rmSync(f, { force: true });
      }
      this.state.delete(id);
      dropLog(id);
      n++;
    }
    if (n) log("main", `deleted ${n} archived task(s)`);
    return n;
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

function hostOf(tracker: Tracker): "github" | "gitlab" | undefined {
  return tracker.name === "GitLab" ? "gitlab" : tracker.name === "GitHub" ? "github" : undefined;
}
