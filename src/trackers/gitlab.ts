import type { ProjectConfig } from "../config.js";
import { requestJson, explainAccess } from "../http.js";
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
  /**
   * Work item status names to set (GitLab 17.11+, Premium/Ultimate), e.g. "In progress";
   * null or missing leaves the status alone.
   */
  transitions?: { onStart?: string | null; onSuccess?: string | null; onFailure?: string | null };
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
    return requestJson<T>(method, `${this.apiUrl}${path}`, this.headers, body).catch((err) =>
      explainAccess(err, (status, text) => {
        const tokens = `${this.apiUrl.replace(/\/api\/v4$/, "")}/-/user_settings/personal_access_tokens`;
        if (status === 401) return `GitLab не прийняв токен (401): він недійсний або прострочений. Створіть новий: ${tokens}`;
        if (status === 403 && text.includes("insufficient_scope"))
          return `Токену GitLab бракує прав: потрібен scope «api», бо Mako читає задачі й пише коментарі та мітки. Створіть токен зі scope api: ${tokens}`;
        if (status === 403) return "GitLab заборонив доступ (403): власник токена має бути учасником проєкту з роллю Reporter або вище";
        if (status === 404) return "GitLab не знайшов проєкт (404): перевірте шлях group/project і чи має токен доступ до нього";
        return undefined;
      }),
    );
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
          nativeId: String(issue.id),
        });
      }
    }
    return tasks;
  }

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `${this.issuePath(task)}/notes`, { body: text });
  }

  async onStart(task: Task): Promise<void> {
    await this.setStatus(task, this.cfg.transitions?.onStart);
    await this.applyLabels(task, labelChanges(this.labels, "start"));
  }

  async onFinish(task: Task, outcome: Outcome): Promise<void> {
    const t = this.cfg.transitions;
    await this.setStatus(task, outcome === "success" ? t?.onSuccess : outcome === "failed" ? t?.onFailure : undefined);
    await this.applyLabels(task, labelChanges(this.labels, outcome));
  }

  /** Statuses of the project's issues, from the work item type definition. */
  async listStatuses(): Promise<string[]> {
    const projectPath = Object.keys(this.projects)[0];
    const data = await this.graphql<any>(
      `query($path: ID!) { project(fullPath: $path) { workItemTypes { nodes { name widgetDefinitions {
         type ... on WorkItemWidgetDefinitionStatus { allowedStatuses { name } } } } } } }`,
      { path: projectPath },
    );
    const types: any[] = data?.project?.workItemTypes?.nodes ?? [];
    const issue = types.find((t) => /^issue$/i.test(t.name)) ?? types[0];
    const def = issue?.widgetDefinitions?.find((w: any) => Array.isArray(w.allowedStatuses));
    if (!def) {
      throw new Error("Статуси задач на цьому GitLab недоступні (потрібні GitLab 17.11+ і тариф Premium або Ultimate). Мітки статусу можна ввімкнути в «Додатково».");
    }
    return def.allowedStatuses.map((s: any) => s.name);
  }

  /** Moves the issue to a work item status by name (resolved case-insensitively by GitLab). */
  private async setStatus(task: Task, name: string | null | undefined): Promise<void> {
    if (!name) return;
    // Tasks taken before Mako kept the issue's global id: look it up.
    const id = task.nativeId ?? String((await this.request<any>("GET", this.issuePath(task))).id);
    const data = await this.graphql<any>(
      `mutation($id: WorkItemID!, $name: String!) { workItemUpdate(input: { id: $id, statusWidget: { name: $name } }) { errors } }`,
      { id: `gid://gitlab/WorkItem/${id}`, name },
    );
    const errors: string[] = data?.workItemUpdate?.errors ?? [];
    if (errors.length) throw new Error(`GitLab не змінив статус ${task.id} на «${name}»: ${errors.join("; ")}`);
  }

  private async graphql<T>(query: string, variables: Record<string, unknown>): Promise<T> {
    const url = this.apiUrl.replace(/\/api\/v4$/, "/api/graphql");
    const res = await requestJson<{ data?: T; errors?: { message: string }[] }>("POST", url, this.headers, { query, variables }).catch((err) =>
      explainAccess(err, (status) => (status === 401 ? "GitLab не прийняв токен (401)" : undefined)),
    );
    if (res.errors?.length) {
      const msg = res.errors.map((e) => e.message).join("; ");
      if (/statusWidget|WorkItemWidgetDefinitionStatus|allowedStatuses|doesn't exist on type|not available/i.test(msg)) {
        throw new Error(`Статуси задач на цьому GitLab недоступні (потрібні GitLab 17.11+ і Premium/Ultimate): ${msg}`);
      }
      throw new Error(`GitLab GraphQL: ${msg}`);
    }
    return res.data as T;
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
