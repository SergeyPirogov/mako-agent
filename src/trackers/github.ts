import { execFileSync } from "node:child_process";
import type { ProjectConfig } from "../config.js";
import { requestJson } from "../http.js";
import type { Outcome, Task, Tracker } from "../types.js";
import { defaultLabels, labelChanges, type LabelConfig } from "./labels.js";

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

  constructor(private cfg: GitHubTrackerConfig) {
    this.id = cfg.id ?? "github";
    this.projects = cfg.projects;
    this.apiUrl = (cfg.apiUrl ?? "https://api.github.com").replace(/\/$/, "");
    this.labels = { ...defaultLabels, ...cfg.labels };
    this.headers = { Authorization: `Bearer ${githubToken()}`, "X-GitHub-Api-Version": "2022-11-28" };
  }

  private request<T>(method: string, path: string, body?: unknown): Promise<T> {
    return requestJson<T>(method, `${this.apiUrl}${path}`, this.headers, body);
  }

  async fetchTasks(): Promise<Task[]> {
    const tasks: Task[] = [];
    for (const repo of Object.keys(this.projects)) {
      const query = new URLSearchParams({ state: "open", labels: this.labels.trigger, per_page: "50" });
      if (this.cfg.assignee) query.set("assignee", this.cfg.assignee);
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

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `${this.issuePath(task)}/comments`, { body: text });
  }

  onStart(task: Task): Promise<void> {
    return this.applyLabels(task, labelChanges(this.labels, "start"));
  }

  onFinish(task: Task, outcome: Outcome): Promise<void> {
    return this.applyLabels(task, labelChanges(this.labels, outcome));
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
function githubToken(): string {
  if (process.env.GITHUB_TOKEN) return process.env.GITHUB_TOKEN;
  try {
    return execFileSync("gh", ["auth", "token"], { encoding: "utf8" }).trim();
  } catch {
    throw new Error("GitHub tracker needs GITHUB_TOKEN or a logged-in `gh` CLI");
  }
}
