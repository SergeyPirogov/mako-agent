import type { ProjectConfig } from "../config.js";
import { requestJson } from "../http.js";
import type { Outcome, Task, Tracker } from "../types.js";
import { defaultLabels, labelChanges, type LabelConfig } from "./labels.js";

export interface GitLabTrackerConfig {
  type: "gitlab";
  id?: string;
  /** Defaults to https://gitlab.com. The token comes from env GITLAB_TOKEN (scope: api). */
  baseUrl?: string;
  labels?: Partial<LabelConfig>;
  /** Only issues assigned to this username. */
  assignee?: string;
  /** Keys are project paths: "group/subgroup/project"; every listed project is polled. */
  projects: Record<string, ProjectConfig>;
}

export class GitLabTracker implements Tracker {
  readonly name = "GitLab";
  readonly id: string;
  readonly projects: Record<string, ProjectConfig>;
  private apiUrl: string;
  private labels: LabelConfig;
  private headers: Record<string, string>;

  constructor(private cfg: GitLabTrackerConfig, env: (name: string) => string) {
    this.id = cfg.id ?? "gitlab";
    this.projects = cfg.projects;
    this.apiUrl = (cfg.baseUrl ?? "https://gitlab.com").replace(/\/$/, "") + "/api/v4";
    this.labels = { ...defaultLabels, ...cfg.labels };
    this.headers = { "PRIVATE-TOKEN": env("GITLAB_TOKEN") };
  }

  private request<T>(method: string, path: string, body?: unknown): Promise<T> {
    return requestJson<T>(method, `${this.apiUrl}${path}`, this.headers, body);
  }

  async fetchTasks(): Promise<Task[]> {
    const tasks: Task[] = [];
    for (const projectPath of Object.keys(this.projects)) {
      const query = new URLSearchParams({ state: "opened", labels: this.labels.trigger, per_page: "50" });
      if (this.cfg.assignee) query.set("assignee_username", this.cfg.assignee);
      const issues = await this.request<any[]>("GET", `${projectApi(projectPath)}/issues?${query}`);
      for (const issue of issues) {
        const notes = issue.user_notes_count
          ? await this.request<any[]>("GET", `${projectApi(projectPath)}/issues/${issue.iid}/notes?sort=asc&per_page=100`)
          : [];
        tasks.push({
          id: `${projectPath}#${issue.iid}`,
          slug: `${projectPath.split("/").pop()}-${issue.iid}`,
          trackerId: this.id,
          project: projectPath,
          title: issue.title,
          description: issue.description ?? "",
          url: issue.web_url,
          meta: issue.labels?.length ? `Labels: ${issue.labels.join(", ")}` : undefined,
          labels: issue.labels ?? [],
          components: [],
          comments: notes.filter((n) => !n.system).map((n) => ({ author: n.author?.name ?? "unknown", body: n.body })),
          closingRef: `Closes ${projectPath}#${issue.iid}`,
        });
      }
    }
    return tasks;
  }

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `${this.issuePath(task)}/notes`, { body: text });
  }

  onStart(task: Task): Promise<void> {
    return this.applyLabels(task, labelChanges(this.labels, "start"));
  }

  onFinish(task: Task, outcome: Outcome): Promise<void> {
    return this.applyLabels(task, labelChanges(this.labels, outcome));
  }

  private issuePath(task: Task): string {
    return `${projectApi(task.project)}/issues/${task.id.split("#")[1]}`;
  }

  private async applyLabels(task: Task, { add, remove }: { add: string[]; remove: string[] }): Promise<void> {
    if (!add.length && !remove.length) return;
    await this.request("PUT", this.issuePath(task), { add_labels: add.join(","), remove_labels: remove.join(",") });
  }
}

function projectApi(projectPath: string): string {
  return `/projects/${encodeURIComponent(projectPath)}`;
}
