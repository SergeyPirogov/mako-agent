import type { Config } from "./config.js";
import type { Outcome, Task, Tracker } from "./types.js";
import { StateStore, type TaskRecord } from "./state.js";
import { buildPrompt } from "./prompt.js";
import { createExecutor, type Executor } from "./executors/index.js";
import type { RepoConfig } from "./config.js";
import { existsSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { askToTake, inform, type Decision } from "./notify.js";
import * as git from "./git.js";
import { routeTask } from "./routing.js";
import { LocalTracker, LOCAL_TRACKER, localTask } from "./trackers/local.js";
import { log, keepLogsIn, dropLog } from "./log.js";
import { stopTask } from "./executors/processes.js";
import { taskSummary } from "./commit-message.js";
import { learn } from "./memory.js";
import { ReviewDesk } from "./review-desk.js";
import { TrackerSync } from "./tracker-sync.js";
import { Planning } from "./planning.js";

export class Orchestrator {
  /** By id; replaced on reload. */
  trackers: Map<string, Tracker>;
  executors: Map<string, Executor>;
  readonly reviews = new ReviewDesk(this);
  readonly sync = new TrackerSync(this);
  readonly planning = new Planning(this);
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
    public cfg: Config,
    trackers: Tracker[],
    readonly state: StateStore,
  ) {
    keepLogsIn(path.join(cfg.stateDir, "activity"));
    this.trackers = withLocal(trackers);
    this.executors = new Map(Object.entries(cfg.executors).map(([name, c]) => [name, createExecutor(name, c)]));
  }

  executorFor(repo: RepoConfig): Executor {
    return this.executors.get(repo.executor ?? this.cfg.defaultExecutor)!;
  }

  /** Applies a new config from the settings page. Running agents keep the settings they started with. */
  reload(cfg: Config, trackers: Tracker[]): void {
    this.cfg = cfg;
    this.trackers = withLocal(trackers);
    this.executors = new Map(Object.entries(cfg.executors).map(([name, c]) => [name, createExecutor(name, c)]));
    this.lastHandoffCheck = 0;
    this.schedule();
  }

  async start(): Promise<void> {
    log("main", `Polling ${[...this.trackers.values()].filter((t) => t.id !== LOCAL_TRACKER).map((t) => t.id).join(", ")} every ${this.cfg.pollIntervalSec}s`);
    this.schedule(); // tasks queued before a restart
    this.reviewTimer = setInterval(() => void this.reviews.refreshReviews(), 60_000);
    void this.reviews.refreshReviews();
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
  async updateTracker(task: Task, moment: "start" | Outcome | "merged", change: () => Promise<string | void>): Promise<void> {
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
    this.wake?.(); // the poll loop ends now, not after its pause
  }

  async drain(): Promise<void> {
    await Promise.allSettled(this.active.values());
  }

  /** Tasks taken and not finished: the Kanban "in progress" column that `wipLimit` caps. */
  wip(): number {
    return this.state.all().filter((r) => ["queued", "planned", "running", "handed-off"].includes(r.status)).length;
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
        if (tracker.id === LOCAL_TRACKER) continue; // tasks typed into Mako: nothing to search
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
          const state = await this.sync.issueState(tracker, rec);
          if (state === "closed" || state === "deleted") this.sync.markGone(rec, state);
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
      await this.sync.checkHandoffs();
      await this.sync.checkGone();
    }
  }

  /** PRs/MRs Mako opened: once merged, the issue gets its final status and the card says so. */
  async checkMerged(): Promise<void> {
    await this.reviews.refreshReviews();
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
  decide(id: string, decision: Decision, choice: { repo?: string; executor?: string; planFirst?: boolean } = {}): boolean {
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
        // Unless the developer said otherwise when taking it, the setting decides.
        planFirst: (choice.planFirst ?? this.cfg.planFirst) || undefined,
        ...(chosenRepo ? { repo: chosenRepo } : {}),
      });
      this.schedule();
      void this.sync.assignOnTake(id);
    } else {
      const snoozeMin = this.cfg.notifications.snoozeMin;
      log(id, `${decision === "later" ? "postponed" : "no answer"}, asking again in ${snoozeMin} min`);
      this.state.update(id, { status: "snoozed", snoozedUntil: Date.now() + snoozeMin * 60_000 });
    }
    return true;
  }

  schedule(): void {
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
    // Plan first: Claude in Mako's terminal plans and asks in the terminal itself; any other executor gets a
    // plan from Claude that the developer approves on the board before the work starts.
    const needsPlan = Boolean(rec.planFirst && !rec.planApproved && !rec.followUp);
    const terminalPlan = needsPlan && this.planning.isTerminalClaude(executor.name);
    const context = this.planning.promptContext(rec, repo);
    if (needsPlan && !terminalPlan) return this.planning.planPhase(rec, tracker, repo, branch, buildPrompt(task, tracker, repo, executor) + context);
    // A follow-up (fix CI, review comments, conflicts) continues the work in the same worktree.
    const prompt = rec.followUp
      ? `You are continuing work on ${task.id}: ${task.title} (${task.url}) in this worktree, on branch ${branch}.\n\n${rec.followUp}`
      : buildPrompt(task, tracker, repo, executor) +
        context +
        (rec.planApproved && rec.plan ? `\n\n## Approved plan\n\nThe developer approved this plan. Follow it:\n\n${rec.plan}` : "");
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
          planMode: terminalPlan,
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

      await this.reviews.commitLeftovers(worktree, task);

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
        prUrl = (await this.reviews.openReview(task, tracker, repo, worktree, branch, executor.label)).url ?? prUrl;
        pushedNow = true;
      }
      if (prUrl) await this.reviews.postReplies(task, tracker, worktree, prUrl);

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
      // In the background: the next task in this repo starts from what this one found out.
      if (this.cfg.memory) {
        void learn(this.cfg.stateDir, repo.name, worktree, repo.baseBranch, task, result.summary)
          .then((changed) => changed && log(task.id, `repo notes updated: ${repo.name}`))
          .catch((err: any) => log(task.id, `repo notes not updated: ${err.message}`));
      }
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
  retry(id: string, choice: { repo?: string; executor?: string; resume?: boolean; fresh?: boolean; planFirst?: boolean } = {}): boolean {
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
      // «Спершу план» on a retry: a new plan for this run.
      // A new plan for a run from scratch (or when asked); the same worktree keeps the plan already approved.
      ...((choice.planFirst ?? (choice.fresh && this.cfg.planFirst)) && !resume
        ? { planFirst: true, planApproved: undefined, plan: undefined, planFeedback: undefined }
        : choice.planFirst === false
          ? { planFirst: undefined }
          : {}),
      ...(chosenRepo ? { repo: chosenRepo } : {}),
      // A fresh run, not the old hand-off: forget its branch and PR tracking.
      branch: undefined,
      branchSha: undefined,
    });
    this.schedule();
    return true;
  }

  /** The branch a task runs on: named from the pattern at its first run; tasks from before patterns keep agent/<slug>. */
  branchOf(rec: TaskRecord): string {
    return rec.branchName ?? git.branchFor(rec.task.slug);
  }

  /** Executors for «+ Задача», the default first. */
  executorChoices(): { name: string; label: string; default: boolean }[] {
    return [...this.executors.values()]
      .map((e) => ({ name: e.name, label: e.label, default: e.name === this.cfg.defaultExecutor }))
      .sort((a, b) => Number(b.default) - Number(a.default));
  }

  /** «+ Задача»: a task written in Mako, taken right away (or planned first) in the chosen repo. */
  createTask(input: { title: string; description: string; repo: string; executor?: string; planFirst?: boolean; template?: string }): string {
    const local = this.trackers.get(LOCAL_TRACKER)!;
    if (!input.title.trim()) throw new Error("Вкажіть, що зробити");
    if (!local.projects[input.repo]) throw new Error(`Немає репозиторію «${input.repo}»`);
    const n = 1 + Math.max(0, ...this.state.all().filter((r) => r.task.trackerId === LOCAL_TRACKER).map((r) => Number(r.id.split("#")[1]) || 0));
    const task = localTask(n, input.title.trim(), input.description.trim(), input.repo);
    const executor = input.executor && this.executors.has(input.executor) ? input.executor : undefined;
    log(task.id, `new task from Mako → ${input.repo}: ${task.title}`);
    const template = this.cfg.templates.find((t) => t.name === input.template)?.name;
    this.state.update(task.id, { task, repo: input.repo, status: "snoozed", snoozedUntil: Date.now() + 365 * 24 * 3600_000, template });
    this.decide(task.id, "take", { repo: input.repo, executor, planFirst: input.planFirst });
    return task.id;
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

  // ---- delegated: the web UI talks to the orchestrator only ----

  refreshReviews(): Promise<void> {
    return this.reviews.refreshReviews();
  }
  refreshReview(id: string) {
    return this.reviews.refreshReview(id);
  }
  get reviewChecked(): Map<string, number> {
    return this.reviews.reviewChecked;
  }
  mergeReview(id: string): Promise<void> {
    return this.reviews.mergeReview(id);
  }
  askAgent(id: string, kind: "ci" | "comments" | "conflicts") {
    return this.reviews.askAgent(id, kind);
  }
  publishNow(id: string) {
    return this.reviews.publishNow(id);
  }
  pendingChanges(id: string) {
    return this.reviews.pendingChanges(id);
  }
  checkGone(): Promise<void> {
    return this.sync.checkGone();
  }
  checkHandoffs(): Promise<void> {
    return this.sync.checkHandoffs();
  }
  answerPlan(id: string, answer: "approve" | "revise" | "reject", notes = ""): boolean {
    return this.planning.answerPlan(id, answer, notes);
  }
  memories() {
    return this.planning.memories();
  }
  saveMemory(repo: string, text: string): void {
    this.planning.saveMemory(repo, text);
  }
  templateNames(): string[] {
    return this.planning.templateNames();
  }
}

/** The configured trackers plus Mako's own, for tasks typed in on the board. */
function withLocal(trackers: Tracker[]): Map<string, Tracker> {
  const map = new Map(trackers.map((t) => [t.id, t] as [string, Tracker]));
  map.set(LOCAL_TRACKER, new LocalTracker(trackers));
  return map;
}
