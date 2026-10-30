import { execFileSync } from "node:child_process";
import type { ProjectConfig } from "../config.js";
import { requestJson, explainAccess, HttpError } from "../http.js";
import type { IssueState, Outcome, ReviewInfo, Task, Tracker } from "../types.js";
import { defaultLabels, labelChanges, normalizeAssignee, type LabelConfig } from "./labels.js";

export interface GitHubTrackerConfig {
  type: "github";
  id?: string;
  /** For GitHub Enterprise: https://github.example.com/api/v3 */
  apiUrl?: string;
  labels?: Partial<LabelConfig>;
  /** Only issues assigned to this login (or "none" / "*"). */
  assignee?: string;
  /** Keys are "owner/repo"; every listed repository is polled. */
  projects: Record<string, ProjectConfig>;
}

export class GitHubTracker implements Tracker {
  readonly name = "GitHub";
  readonly id: string;
  readonly projects: Record<string, ProjectConfig>;
  private apiUrl: string;
  private labels: LabelConfig;
  private headers: Record<string, string>;

  constructor(private cfg: GitHubTrackerConfig, env: (name: string) => string) {
    this.id = cfg.id ?? "github";
    this.projects = cfg.projects;
    this.apiUrl = (cfg.apiUrl ?? "https://api.github.com").replace(/\/$/, "");
    this.labels = { ...defaultLabels, ...cfg.labels };
    if (!this.labels.trigger && !normalizeAssignee(cfg.assignee)) {
      throw new Error(`${this.name}: без мітки для агента вкажіть «Лише призначені на» (ваш нік або me), інакше Mako братиме всі задачі проєкту`);
    }
    this.headers = { Authorization: `Bearer ${githubToken(env)}`, "X-GitHub-Api-Version": "2022-11-28" };
  }

  private request<T>(method: string, path: string, body?: unknown): Promise<T> {
    return requestJson<T>(method, `${this.apiUrl}${path}`, this.headers, body).catch((err) =>
      explainAccess(err, (status) => {
        if (status === 401) return "GitHub не прийняв токен (401): він недійсний або прострочений. Оновіть GITHUB_TOKEN або виконайте `gh auth login`";
        if (status === 403 || status === 404)
          return `GitHub не дає доступу до репозиторію (${status}): перевірте назву owner/repo і права токена. Fine-grained token: Issues → Read and write; classic: scope repo`;
        return undefined;
      }),
    );
  }

  async fetchTasks(): Promise<Task[]> {
    const tasks: Task[] = [];
    for (const repo of Object.keys(this.projects)) {
      const query = new URLSearchParams({ state: "open", per_page: "50" });
      if (this.labels.trigger) query.set("labels", this.labels.trigger);
      const assignee = await this.assignee();
      if (assignee) query.set("assignee", assignee);
      const issues = await this.request<any[]>("GET", `/repos/${repo}/issues?${query}`);
      for (const issue of issues.filter((i) => !i.pull_request)) {
        const comments = issue.comments
          ? await this.request<any[]>("GET", `/repos/${repo}/issues/${issue.number}/comments?per_page=100`)
          : [];
        tasks.push({
          id: `${repo}#${issue.number}`,
          slug: `${repo.split("/")[1]}-${issue.number}`,
          trackerId: this.id,
          project: repo,
          title: issue.title,
          description: issue.body ?? "",
          url: issue.html_url,
          meta: issue.labels?.length ? `Labels: ${issue.labels.map((l: any) => l.name).join(", ")}` : undefined,
          labels: (issue.labels ?? []).map((l: any) => l.name),
          components: [],
          comments: comments.map((c) => ({ author: c.user?.login ?? "unknown", body: c.body ?? "" })),
          closingRef: `Closes ${repo}#${issue.number}`,
        });
      }
    }
    return tasks;
  }

  private me?: string;

  /** The configured assignee; "me" is resolved once to the token owner's login. */
  private async assignee(): Promise<string | undefined> {
    const a = normalizeAssignee(this.cfg.assignee);
    if (a?.toLowerCase() !== "me") return a;
    this.me ??= (await this.request<{ login: string }>("GET", "/user")).login;
    return this.me;
  }

  async describeFilter(): Promise<string> {
    const who = await this.assignee();
    return [
      "відкриті issues",
      this.labels.trigger ? `з міткою «${this.labels.trigger}»` : "без вимоги мітки",
      who ? `призначені на ${who}` : "призначені на будь-кого",
      `у ${Object.keys(this.projects).join(", ")}`,
    ].join(" ");
  }

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `${this.issuePath(task)}/comments`, { body: text });
  }

  onStart(task: Task): Promise<void> {
    return this.applyLabels(task, labelChanges(this.labels, "start"));
  }

  onFinish(task: Task, outcome: Outcome): Promise<void> {
    return this.applyLabels(task, labelChanges(this.labels, outcome));
  }

  private prPath(reviewUrl: string): { repo: string; path: string } {
    const m = reviewUrl.match(/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/);
    if (!m) throw new Error(`не схоже на PR GitHub: ${reviewUrl}`);
    return { repo: m[1], path: `/repos/${m[1]}/pulls/${m[2]}` };
  }

  async reviewDetails(reviewUrl: string): Promise<ReviewInfo> {
    const { repo, path } = this.prPath(reviewUrl);
    const pr = await this.request<any>("GET", path);
    const state = pr.merged ? "merged" : pr.state === "closed" ? "closed" : "open";
    const runs: any[] = (await this.request<any>("GET", `/repos/${repo}/commits/${pr.head.sha}/check-runs?per_page=50`).catch(() => ({}))).check_runs ?? [];
    const failed = runs.filter((r) => ["failure", "timed_out", "cancelled", "action_required"].includes(r.conclusion));
    const pipeline = runs.length
      ? {
          status: failed.length ? "failed" : runs.some((r) => r.status !== "completed") ? "running" : "success",
          url: `${pr.html_url}/checks`,
          failed: failed.map((r) => ({ name: r.name, url: r.html_url })),
        }
      : undefined;
    // REST has no "resolved" flag on review threads: the top-level review comments stand for them.
    const raw: any[] = state === "open" ? await this.request<any[]>("GET", `${path}/comments?per_page=100`).catch(() => []) : [];
    const comments = raw
      .filter((c) => !c.in_reply_to_id)
      .map((c) => ({
        author: c.user?.login ?? "reviewer",
        body: c.body,
        path: c.path,
        line: c.line ?? c.original_line ?? undefined,
        url: c.html_url,
        replies: raw.filter((r) => r.in_reply_to_id === c.id).length,
      }));
    return {
      state,
      conflicts: pr.mergeable === false || pr.mergeable_state === "dirty",
      mergeable: state === "open" && pr.mergeable === true && ["clean", "has_hooks", "unstable"].includes(pr.mergeable_state),
      blocker: state === "open" && pr.mergeable_state !== "clean" ? pr.mergeable_state : undefined,
      pipeline,
      comments,
      checkedAt: Date.now(),
    };
  }

  async mergeReview(reviewUrl: string): Promise<void> {
    await this.request("PUT", `${this.prPath(reviewUrl).path}/merge`, {}).catch((err: any) => {
      throw new Error(`GitHub не змерджив PR: ${String(err.message).split("\n")[0]}`);
    });
  }

  async reviewState(reviewUrl: string): Promise<"open" | "merged" | "closed"> {
    const m = reviewUrl.match(/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/);
    if (!m) throw new Error(`не схоже на PR GitHub: ${reviewUrl}`);
    const pr = await this.request<{ state: string; merged: boolean }>("GET", `/repos/${m[1]}/pulls/${m[2]}`);
    return pr.merged ? "merged" : pr.state === "closed" ? "closed" : "open";
  }

  async describeReview(reviewUrl: string, title: string, description: string): Promise<void> {
    const m = reviewUrl.match(/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/);
    if (!m) throw new Error(`не схоже на PR GitHub: ${reviewUrl}`);
    await this.request("PATCH", `/repos/${m[1]}/pulls/${m[2]}`, { title, body: description });
  }

  async issueState(task: Task): Promise<IssueState> {
    try {
      const issue = await requestJson<any>("GET", `${this.apiUrl}${this.issuePath(task)}`, this.headers);
      return issue.state === "closed" ? "closed" : "open";
    } catch (err) {
      if (err instanceof HttpError && (err.status === 404 || err.status === 410)) return "deleted";
      throw err;
    }
  }

  private issuePath(task: Task): string {
    return `/repos/${task.project}/issues/${task.id.split("#")[1]}`;
  }

  private async applyLabels(task: Task, { add, remove }: { add: string[]; remove: string[] }): Promise<void> {
    for (const label of remove) {
      await this.request("DELETE", `${this.issuePath(task)}/labels/${encodeURIComponent(label)}`).catch(() => {});
    }
    if (add.length) await this.request("POST", `${this.issuePath(task)}/labels`, { labels: add });
  }
}

/** GITHUB_TOKEN from env, otherwise the token the GitHub CLI is logged in with. */
function githubToken(env: (name: string) => string): string {
  try {
    return env("GITHUB_TOKEN");
  } catch {
    // fall back to the GitHub CLI login
  }
  try {
    return execFileSync("gh", ["auth", "token"], { encoding: "utf8" }).trim();
  } catch {
    throw new Error("GitHub tracker needs GITHUB_TOKEN or a logged-in `gh` CLI");
  }
}
