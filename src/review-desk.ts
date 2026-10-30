import { existsSync, readFileSync, rmSync } from "node:fs";
import path from "node:path";
import type { RepoConfig } from "./config.js";
import type { Task, Tracker } from "./types.js";
import type { TaskRecord } from "./state.js";
import type { Orchestrator } from "./orchestrator.js";
import { createTracker, type TrackerConfig } from "./trackers/index.js";
import * as git from "./git.js";
import { commitMessage, reviewText } from "./commit-message.js";
import { inform } from "./notify.js";
import { log } from "./log.js";
import { sessions, sessionIdFor } from "./sessions.js";

/** Where the agent leaves its answers to review threads; kept out of git by .git/info/exclude. */
export const REVIEW_REPLIES = ".mako/review-replies.json";

/**
 * Everything about the PRs/MRs Mako opens: pushing and describing them, watching CI, review threads,
 * conflicts and merges, handing the agent what they need, and answering the threads.
 */
export class ReviewDesk {
  constructor(private o: Orchestrator) {}

  /** PRs/MRs Mako opened and still watches: open, not archived, recent. */
  watchedReviews(): TaskRecord[] {
    const recent = Date.now() - 30 * 24 * 3600_000;
    return this.o.state
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
    const rec = this.o.state.get(id);
    const tracker = rec && this.o.trackers.get(rec.task.trackerId);
    if (!rec?.prUrl || !tracker) return rec;
    try {
      const host = this.reviewHost(rec.prUrl, tracker);
      if (host?.reviewDetails) {
        const review = await host.reviewDetails(rec.prUrl);
        const before = rec.review;
        this.reviewChecked.set(id, Date.now());
        // Only real changes are saved: each save redraws the board.
        const same = before && JSON.stringify({ ...before, checkedAt: 0 }) === JSON.stringify({ ...review, checkedAt: 0 });
        if (!same) this.o.state.update(id, { review });
        if (review.pipeline?.status === "failed" && before?.pipeline?.status !== "failed") {
          inform(`❌ ${id}: CI впав`, `${review.pipeline.failed.map((j) => j.name).join(", ") || "пайплайн"} · можна віддати агенту`);
        }
        if (review.conflicts && !before?.conflicts) inform(`⚠ ${id}: конфлікти з основною гілкою`, rec.task.title);
        if (review.comments.length > (before?.comments.length ?? 0)) {
          inform(`💬 ${id}: нові коментарі в ${this.reviewKind(rec)}`, review.comments.at(-1)!.body.slice(0, 160));
        }
        if (review.state !== "open") await this.reviewEnded(this.o.state.get(id)!, review.state, tracker);
      } else if (host?.reviewState) {
        const state = await host.reviewState(rec.prUrl);
        if (state !== "open") await this.reviewEnded(rec, state, tracker);
      }
    } catch (err: any) {
      log(id, `review check failed: ${err.message}`);
    }
    return this.o.state.get(id);
  }

  private hosts = new Map<string, Tracker>();

  /**
   * Who answers about a PR/MR: the task's own tracker when it is that GitLab/GitHub, otherwise a client for the
   * MR's host with the GitLab/GitHub token from the settings, so Jira tasks with code in GitLab get CI,
   * review threads and merge too.
   */
  reviewHost(url: string, tracker: Tracker): Tracker | undefined {
    const gitlab = /\/-\/merge_requests\/\d+/.test(url);
    const github = /\/pull\/\d+/.test(url);
    if ((gitlab && tracker.name === "GitLab") || (github && tracker.name === "GitHub")) return tracker;
    const origin = new URL(url).origin;
    const configured = [...this.o.trackers.values()].find(
      (t) => t.name === (gitlab ? "GitLab" : "GitHub") && (this.o.cfg.trackers as any[]).some((c) => (c.id ?? c.type) === t.id && hostOf(c, gitlab) === origin),
    );
    if (configured) return configured;
    const key = `${gitlab ? "gitlab" : "github"} ${origin}`;
    if (!this.hosts.has(key)) {
      const env = (name: string) => {
        const v = process.env[name];
        if (!v) throw new Error(`Для MR потрібен ${name}: додайте токен ${gitlab ? "GitLab" : "GitHub"} у Налаштуваннях`);
        return v;
      };
      try {
        const cfg = gitlab
          ? { type: "gitlab" as const, baseUrl: origin, projects: {} }
          : { type: "github" as const, apiUrl: origin === "https://github.com" ? undefined : `${origin}/api/v3`, projects: {} };
        this.hosts.set(key, createTracker(cfg as TrackerConfig, env));
      } catch (err: any) {
        log("main", `no client for ${origin}: ${err.message}`);
        return undefined;
      }
    }
    return this.hosts.get(key);
  }

  /** Which host opens the MR when the repo says "auto": the tracker's own kind, or (Jira) a guess from the token. */
  hostHint(tracker: Tracker): "github" | "gitlab" | undefined {
    if (tracker.name === "GitLab") return "gitlab";
    if (tracker.name === "GitHub") return "github";
    return undefined;
  }

  reviewKind(rec: TaskRecord): string {
    return rec.prUrl?.includes("/merge_requests/") ? "MR" : "PR";
  }

  async reviewEnded(rec: TaskRecord, state: "merged" | "closed", tracker: Tracker): Promise<void> {
    if (rec.reviewState) return;
    this.o.state.update(rec.id, { reviewState: state, ...(state === "merged" ? { mergedAt: new Date().toISOString() } : {}) });
    if (state === "closed") return log(rec.id, "review closed without merging");
    log(rec.id, `review merged: ${rec.prUrl}`);
    if (tracker.onMerged) await this.o.updateTracker(rec.task, "merged", () => tracker.onMerged!(rec.task));
    inform(`✅ ${rec.id}: ${this.reviewKind(rec)} змерджено`, rec.status === "running" ? "Агент ще в терміналі: натисніть «Завершити»" : rec.task.title);
  }

  /** Merges the PR/MR from Mako; the issue then moves on as for a merge done in GitLab. */
  async mergeReview(id: string): Promise<void> {
    const rec = this.o.state.get(id);
    const tracker = rec && this.o.trackers.get(rec.task.trackerId);
    const host = rec?.prUrl && tracker ? this.reviewHost(rec.prUrl, tracker) : undefined;
    if (!rec?.prUrl || !host?.mergeReview) throw new Error("Немає MR, який Mako може змерджити");
    await host.mergeReview(rec.prUrl);
    log(id, `merged from Mako: ${rec.prUrl}`);
    await this.refreshReview(id);
  }

  /**
   * Hands the agent what the PR/MR needs: the failed CI jobs with their logs, the open review threads,
   * or the conflicts. A live terminal gets it as a message; otherwise a new run starts in the same worktree.
   */
  async askAgent(id: string, kind: "ci" | "comments" | "conflicts"): Promise<"sent" | "started"> {
    let rec = await this.refreshReview(id);
    const tracker = rec && this.o.trackers.get(rec.task.trackerId);
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
        const host = this.reviewHost(rec.prUrl, tracker);
        const log = job.id && host?.jobLog ? await host.jobLog(rec.prUrl, job.id).catch(() => "") : "";
        logs.push(`### ${job.name}${job.url ? ` (${job.url})` : ""}\n${log ? "```\n" + log.trim() + "\n```" : "(log not available)"}`);
      }
      text = `The CI pipeline of ${kindName} ${rec.prUrl} failed${review.pipeline.url ? ` (${review.pipeline.url})` : ""}.\n\n${logs.join("\n\n") || "No failed job details were available."}\n\nFind the cause, fix it in this worktree, run the same checks locally where you can, and commit.${tail}`;
    } else if (kind === "comments") {
      if (!review.comments.length) throw new Error("Відкритих коментарів немає");
      const list = review.comments
        .map((c, i) => `${i + 1}. ${c.thread ? `[thread ${c.thread}] ` : ""}${c.author}${c.path ? ` on ${c.path}${c.line ? `:${c.line}` : ""}` : ""}${c.url ? ` (${c.url})` : ""}:\n${c.body.trim().split("\n").map((l) => `> ${l}`).join("\n")}`)
        .join("\n\n");
      const ws = this.o.workspaceFor(id);
      if (ws) await git.excludeLocal(ws.worktree, ".mako/");
      text =
        `Reviewers left unresolved comments on ${kindName} ${rec.prUrl}:\n\n${list}\n\n` +
        "Address each one in the code. If you disagree with a comment, do not change the code for it. Commit the changes.\n\n" +
        `Then write the file \`${REVIEW_REPLIES}\` in the worktree root (it is not committed): a JSON array with one entry per thread, ` +
        '`{"thread": "<id from [thread …]>", "reply": "<what you changed, with file names, or why you did not>", "resolved": <true if fixed>}`. ' +
        `Mako posts these replies in the threads after it pushes, and resolves the fixed ones.${tail}`;
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
    this.o.state.update(id, { followUp: text });
    if (!this.o.retry(id, { executor: this.o.terminalExecutor() })) throw new Error("Задачу зараз не можна перезапустити");
    log(id, `follow-up run: ${kind}`);
    rec = this.o.state.get(id);
    return "started";
  }

  /** Uncommitted work gets a commit message written from its diff. */
  async commitLeftovers(worktree: string, task: Task): Promise<{ text: string; error?: string } | undefined> {
    await git.excludeLocal(worktree, ".mako/").catch(() => {}); // Mako's own files never go into a commit
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
  async openReview(
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
    const url = await git.publish(repo, worktree, branch, { title: text.title, body: description, shortBody: closing }, this.hostHint(tracker));
    let error = text.error;
    const host = url ? this.reviewHost(url, tracker) : undefined;
    if (url && host?.describeReview) {
      // Push options take one line only: the full description goes through the host's API.
      await host.describeReview(url, text.title, description).catch((err: any) => {
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
  async publishNow(id: string): Promise<{ url?: string; message?: string; title: string; updated: boolean; replied: number; warning?: string }> {
    const rec = this.o.state.get(id);
    const ws = this.o.workspaceFor(id);
    const tracker = rec && this.o.trackers.get(rec.task.trackerId);
    const repo = rec?.repo ? tracker?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    if (!rec || !ws || !tracker || !repo) throw new Error("Для цієї задачі ще немає робочої копії");
    const { task } = rec;
    const message = await this.commitLeftovers(ws.worktree, task);
    if ((await git.commitsAhead(repo, ws.worktree)) === 0) throw new Error("Змін для MR ще немає: агент нічого не змінив");
    const first = !rec.prUrl;
    const branch = this.o.branchOf(rec);
    const agent = (rec.executor && this.o.executors.get(rec.executor)?.label) ?? "агент";
    const review = await this.openReview(task, tracker, repo, ws.worktree, branch, agent);
    const url = review.url ?? rec.prUrl;
    this.o.state.update(id, { prUrl: url, publishedSha: await git.headSha(ws.worktree) });
    const replied = url ? await this.postReplies(task, tracker, ws.worktree, url) : 0;
    log(id, `${first ? "review opened" : "review updated"}${url ? `: ${url}` : ` (branch ${branch} pushed)`}`);
    if (first && url) {
      // The same kind of report the issue gets when a task is finished: what the MR changes and where it is.
      const kind = tracker.name === "GitHub" ? "pull request" : "merge request";
      const report = [`🦈 Mako opened a ${kind}: ${url}`, `**${review.title}**`, review.summary].filter(Boolean).join("\n\n");
      await tracker
        .comment(task, report)
        .then(() => this.o.state.update(id, { reviewAnnounced: true }))
        .catch((err: any) => log(id, `comment not posted: ${err.message}`));
      await this.o.updateTracker(task, "success", () => tracker.onFinish(task, "success"));
    }
    const problems = [message?.error && message.error !== "немає змін" ? `коміт: ${message.error}` : "", review.error ? `MR: ${review.error}` : ""].filter(Boolean);
    return { url, message: message?.text, title: review.title, updated: !first, replied, warning: problems.join("; ") || undefined };
  }

  /**
   * Whether «Створити/Оновити MR» has anything to do: uncommitted work, or commits that are not in the
   * PR/MR yet (since its last push), or, before there is one, any change against the base.
   */
  async pendingChanges(id: string): Promise<{ hasChanges: boolean; pending: boolean }> {
    const rec = this.o.state.get(id);
    const ws = this.o.workspaceFor(id);
    const repo = rec?.repo ? this.o.trackers.get(rec.task.trackerId)?.projects[rec.task.project]?.repos[rec.repo] : undefined;
    if (!rec || !ws || !repo) return { hasChanges: false, pending: false };
    try {
      const dirty = await git.hasUncommittedChanges(ws.worktree);
      const ahead = (await git.commitsAhead(repo, ws.worktree)) > 0;
      const hasChanges = dirty || ahead;
      // Replies the agent wrote for review threads count too: «Оновити MR» posts them.
      const replies = existsSync(path.join(ws.worktree, REVIEW_REPLIES));
      const pending = dirty || replies || (rec.publishedSha ? (await git.headSha(ws.worktree)) !== rec.publishedSha : ahead);
      return { hasChanges, pending };
    } catch {
      return { hasChanges: false, pending: false };
    }
  }

  /**
   * Posts the agent's answers to review threads (written to REVIEW_REPLIES) once the fixes are pushed,
   * and resolves the threads it fixed. Returns how many replies were posted.
   */
  async postReplies(task: Task, tracker: Tracker, worktree: string, url: string): Promise<number> {
    const file = path.join(worktree, REVIEW_REPLIES);
    if (!existsSync(file)) return 0;
    const host = this.reviewHost(url, tracker);
    if (!host?.replyToThread) return 0;
    let entries: { thread?: unknown; reply?: unknown; resolved?: unknown }[];
    try {
      const parsed = JSON.parse(readFileSync(file, "utf8"));
      entries = Array.isArray(parsed) ? parsed : [];
    } catch (err: any) {
      log(task.id, `review replies not read: ${err.message}`);
      return 0;
    }
    const sha = (await git.headSha(worktree).catch(() => "")).slice(0, 8);
    let posted = 0;
    for (const e of entries) {
      if (typeof e.thread !== "string" && typeof e.thread !== "number") continue;
      if (typeof e.reply !== "string" || !e.reply.trim()) continue;
      const body = `🦈 ${e.reply.trim()}${sha && e.resolved === true ? `\n\n_${sha}_` : ""}`;
      try {
        await host.replyToThread(url, String(e.thread), body, e.resolved === true);
        posted++;
      } catch (err: any) {
        log(task.id, `reply to thread ${e.thread} not posted: ${err.message}`);
      }
    }
    rmSync(file, { force: true });
    log(task.id, `replied in ${posted} review thread(s)`);
    return posted;
  }
}

/** The web origin of a configured GitLab/GitHub tracker, to match it with an MR's URL. */
function hostOf(cfg: any, gitlab: boolean): string {
  try {
    if (gitlab) return new URL(cfg.baseUrl ?? "https://gitlab.com").origin;
    return cfg.apiUrl ? new URL(cfg.apiUrl).origin : "https://github.com";
  } catch {
    return "";
  }
}
