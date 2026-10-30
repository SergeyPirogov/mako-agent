import { execFileSync } from "node:child_process";
import type { ProjectConfig } from "../config.js";
import { requestJson, requestFile, explainAccess, HttpError } from "../http.js";
import type { Attachment, IssueState, Outcome, ReviewInfo, Task, Tracker } from "../types.js";
import { linkedFiles } from "./attachments.js";
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
  /** Also every issue assigned to me in any repository: Mako sets up (and clones) the repository when it sees one. */
  allProjects?: boolean;
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
    if (!this.labels.trigger && !normalizeAssignee(cfg.assignee) && !cfg.allProjects) {
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
    if (this.cfg.allProjects) {
      const query = new URLSearchParams({ filter: "assigned", state: "open", per_page: "50" });
      if (this.labels.trigger) query.set("labels", this.labels.trigger);
      for (const issue of await this.request<any[]>("GET", `/issues?${query}`)) {
        const repo = issue.repository?.full_name as string | undefined;
        if (!repo || issue.pull_request || this.projects[repo]) continue;
        const comments = issue.comments ? await this.request<any[]>("GET", `/repos/${repo}/issues/${issue.number}/comments?per_page=100`) : [];
        tasks.push(this.toTask(repo, issue, comments));
      }
    }
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
        tasks.push(this.toTask(repo, issue, comments));
      }
    }
    return tasks;
  }

  async browseTasks(): Promise<Task[]> {
    const tasks: Task[] = [];
    if (this.cfg.allProjects) {
      for (const issue of await this.request<any[]>("GET", "/issues?filter=assigned&state=open&sort=updated&per_page=100")) {
        const repo = issue.repository?.full_name as string | undefined;
        if (repo && !issue.pull_request && !this.projects[repo]) tasks.push(this.toTask(repo, issue));
      }
    }
    for (const repo of Object.keys(this.projects)) {
      const issues = await this.request<any[]>("GET", `/repos/${repo}/issues?state=open&sort=updated&per_page=100`);
      tasks.push(...issues.filter((i) => !i.pull_request).map((i) => this.toTask(repo, i)));
    }
    return tasks;
  }

  async inspectTask(ref: string): Promise<{ task: Task; misses: string[] } | undefined> {
    const repos = Object.keys(this.projects);
    const m =
      ref.match(/^https?:\/\/[^/]+\/([^/]+\/[^/]+)\/issues\/(\d+)/) ??
      ref.match(/^([\w.-]+\/[\w.-]+)#(\d+)$/) ??
      (repos.length === 1 ? ref.match(/^()#?(\d+)$/) : null);
    if (!m) return undefined;
    const repo = m[1] || repos[0];
    const issue = await this.request<any>("GET", `/repos/${repo}/issues/${m[2]}`);
    const comments = issue.comments ? await this.request<any[]>("GET", `/repos/${repo}/issues/${m[2]}/comments?per_page=100`) : [];
    const task = this.toTask(repo, issue, comments);
    const misses: string[] = [];
    if (!this.projects[repo] && !this.cfg.allProjects) misses.push(`репозиторій ${repo} не доданий у Налаштуваннях (є: ${repos.join(", ")}); або увімкніть «Мої задачі з усіх проєктів»`);
    if (issue.state !== "open") misses.push("issue закрите");
    if (issue.pull_request) misses.push("це pull request, а не issue");
    if (this.labels.trigger && !task.labels.includes(this.labels.trigger)) misses.push(`немає мітки «${this.labels.trigger}» (є: ${task.labels.join(", ") || "жодної"})`);
    const who = await this.assignee();
    if (who && !(task.assignees ?? []).some((a) => a.toLowerCase() === who.toLowerCase())) {
      misses.push(`не призначене на ${who} (виконавці: ${task.assignees?.join(", ") || "немає"})`);
    }
    return { task, misses };
  }

  async downloadAttachment(a: Attachment, maxBytes: number): Promise<Buffer> {
    return requestFile(a.url, { ...this.headers, Accept: "*/*" }, maxBytes);
  }

  private toTask(repo: string, issue: any, comments: any[] = []): Task {
    const task = this.taskOf(repo, issue, comments);
    // Files dropped into an issue: github.com/user-attachments, …/assets, user-images.githubusercontent.com.
    const ours = (url: string) =>
      /^https:\/\/(github\.com\/(user-attachments\/|[^/]+\/[^/]+\/assets\/)|(private-)?user-images\.githubusercontent\.com\/)/.test(url) ? url : undefined;
    return { ...task, attachments: linkedFiles(task, ours) };
  }

  private taskOf(repo: string, issue: any, comments: any[]): Task {
    return {
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
      assignees: (issue.assignees ?? []).map((a: any) => a.login),
    };
  }

  private me?: string;

  /** The configured assignee; "me" is resolved once to the token owner's login. */
  private async assignee(): Promise<string | undefined> {
    // «Мої задачі з усіх проєктів» without a label: my issues in the listed projects too.
    const a = normalizeAssignee(this.cfg.assignee) ?? (this.cfg.allProjects && !this.labels.trigger ? "me" : undefined);
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
          jobs: runs.map((r) => ({
            name: r.name,
            status: r.status !== "completed" ? (r.status === "queued" ? "pending" : "running") : r.conclusion === "success" ? "success" : r.conclusion === "skipped" || r.conclusion === "neutral" ? "skipped" : r.conclusion === "cancelled" ? "canceled" : "failed",
            url: r.html_url,
          })),
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
        thread: String(c.id),
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

  /** GitHub's REST API cannot resolve a review thread: the reply says what was done, the reviewer resolves it. */
  async replyToThread(reviewUrl: string, thread: string, body: string): Promise<void> {
    await this.request("POST", `${this.prPath(reviewUrl).path}/comments/${encodeURIComponent(thread)}/replies`, { body });
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

  async readReview(reviewUrl: string): Promise<{ title: string; description: string }> {
    const m = reviewUrl.match(/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/);
    if (!m) throw new Error(`не схоже на PR GitHub: ${reviewUrl}`);
    const pr = await this.request<{ title: string; body: string | null }>("GET", `/repos/${m[1]}/pulls/${m[2]}`);
    return { title: pr.title, description: pr.body ?? "" };
  }

  async describeReview(reviewUrl: string, title: string, description: string): Promise<void> {
    const m = reviewUrl.match(/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/);
    if (!m) throw new Error(`не схоже на PR GitHub: ${reviewUrl}`);
    await this.request("PATCH", `/repos/${m[1]}/pulls/${m[2]}`, { title, body: description });
  }

  async listProjects(): Promise<{ key: string; name: string }[]> {
    const list = await this.request<{ full_name: string; description?: string | null }[]>("GET", "/user/repos?sort=pushed&per_page=100");
    return list.map((r) => ({ key: r.full_name, name: r.description ? `${r.full_name} — ${r.description.slice(0, 60)}` : r.full_name }));
  }

  private login?: Promise<string>;

  async whoAmI(): Promise<string> {
    this.login ??= this.request<{ login: string }>("GET", "/user").then((u) => u.login);
    return this.login;
  }

  async assignToMe(task: Task): Promise<void> {
    await this.request("POST", `${this.issuePath(task)}/assignees`, { assignees: [await this.whoAmI()] });
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
