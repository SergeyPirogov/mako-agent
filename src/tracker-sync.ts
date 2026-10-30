import type { IssueState, Tracker } from "./types.js";
import type { TaskRecord } from "./state.js";
import type { Orchestrator } from "./orchestrator.js";
import { existsSync } from "node:fs";
import { findReview } from "./reviews.js";
import { inform } from "./notify.js";
import { log } from "./log.js";

/** Keeping Mako and the tracker in step: who the task is assigned to, issues closed or deleted, hand-offs that got a PR. */
export class TrackerSync {
  constructor(private o: Orchestrator) {}

  /** «Взяти» puts the issue on the developer in the tracker, so colleagues do not take it too (assignOnTake: false turns it off). */
  async assignOnTake(id: string): Promise<void> {
    const rec = this.o.state.get(id);
    const tracker = rec && this.o.trackers.get(rec.task.trackerId);
    if (!rec || !tracker?.assignToMe) return;
    const raw = (this.o.cfg.trackers as any[]).find((t) => (t.id ?? t.type) === tracker.id);
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
    for (const rec of this.o.state.all().filter((r) => ["queued", "running", "handed-off"].includes(r.status))) {
      const tracker = this.o.trackers.get(rec.task.trackerId);
      if (!tracker) continue;
      const state = await this.issueState(tracker, rec);
      if ((state === "closed" || state === "deleted") && rec.gone !== state) this.markGone(rec, state);
      if (state === "open" && rec.gone) this.o.state.update(rec.id, { gone: undefined });
    }
  }

  /** undefined when the tracker could not be asked (network, rights): better to wait than to drop a task. */
  async issueState(tracker: Tracker, rec: TaskRecord): Promise<IssueState | undefined> {
    if (!tracker.issueState) return "open";
    try {
      return await tracker.issueState(rec.task);
    } catch (err: any) {
      log(rec.id, `issue check failed: ${err.message}`);
      return undefined;
    }
  }

  /** Not yet started: nothing to do any more. Already with an agent: the developer decides, the card says why. */
  markGone(rec: TaskRecord, state: "closed" | "deleted"): void {
    const where = this.o.trackers.get(rec.task.trackerId)?.name ?? "трекері";
    const what = state === "deleted" ? `Задачу видалено в ${where}` : `Задачу закрито в ${where}`;
    log(rec.id, `${state} in the tracker`);
    if (["asking", "snoozed", "queued"].includes(rec.status)) {
      this.o.state.update(rec.id, { status: "cancelled", gone: state, error: what, finishedAt: new Date().toISOString() });
      this.o.schedule();
    } else {
      this.o.state.update(rec.id, { gone: state });
      inform(`🦈 ${rec.id}: ${what.toLowerCase()}`, "Агент ще працює над нею. Зупиніть або доведіть до кінця на дошці Mako.");
    }
  }

  /** A handed-off task is finished from Mako's side once a PR/MR is opened from its branch. */
  async checkHandoffs(): Promise<void> {
    for (const rec of this.o.state.all().filter((r) => r.status === "handed-off")) {
      const tracker = this.o.trackers.get(rec.task.trackerId);
      const repo = rec.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
      if (!tracker || !repo) continue;
      const branch = rec.branch ?? this.o.branchOf(rec);
      try {
        const { review, branchSha } = await findReview(repo, branch, rec.branchSha);
        if (branchSha && branchSha !== rec.branchSha) this.o.state.update(rec.id, { branchSha });
        if (!review) {
          // The tool's workspace is gone and nothing was opened from it: the hand-off was abandoned.
          if (rec.worktree && !existsSync(rec.worktree)) {
            this.o.cancel(rec.id, `Workspace видалено (${rec.worktree}), PR не відкрито`);
            inform(`🦈 ${rec.id}: передачу скасовано`, "Workspace видалено, PR немає. Задачу можна повторити з дошки.");
          }
          continue;
        }

        const name = review.kind === "pull" ? `Pull request #${review.number}` : `Merge request !${review.number}`;
        const link = review.url ?? name;
        log(rec.id, `review found: ${link}`);
        await tracker.comment(rec.task, `🦈 Mako: ${name} opened for this issue: ${link}`);
        await this.o.updateTracker(rec.task, "success", () => tracker.onFinish(rec.task, "success"));
        this.o.state.update(rec.id, { status: "done", prUrl: review.url, finishedAt: new Date().toISOString() });
        inform(`✅ ${rec.id}: є ${review.kind === "pull" ? "PR" : "MR"}`, link);
      } catch (err: any) {
        log(rec.id, `review check failed: ${err.message}`);
      }
    }
  }
}
