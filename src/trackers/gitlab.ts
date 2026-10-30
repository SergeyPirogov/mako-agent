import type { ProjectConfig } from "../config.js";
import { requestJson, requestText, explainAccess, HttpError } from "../http.js";
import type { IssueState, Outcome, ReviewInfo, Task, Tracker } from "../types.js";
import { defaultLabels, labelChanges, normalizeAssignee, type LabelConfig } from "./labels.js";

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
  transitions?: { onStart?: string | null; onSuccess?: string | null; onFailure?: string | null; onMerged?: string | null };
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
    if (!this.labels.trigger && !normalizeAssignee(cfg.assignee)) {
      throw new Error(`${this.name}: без мітки для агента вкажіть «Лише призначені на» (ваш нік або me), інакше Mako братиме всі задачі проєкту`);
    }
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
      const query = new URLSearchParams({ state: "opened", per_page: "50" });
      if (this.labels.trigger) query.set("labels", this.labels.trigger);
      const assignee = await this.assignee();
      if (assignee) query.set("assignee_username", assignee);
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
          assignees: (issue.assignees ?? (issue.assignee ? [issue.assignee] : [])).map((a: any) => a.username),
          nativeId: String(issue.id),
        });
      }
    }
    return tasks;
  }

  private me?: string;

  /** The configured assignee; "me" is resolved once to the token owner's username. */
  private async assignee(): Promise<string | undefined> {
    const a = normalizeAssignee(this.cfg.assignee);
    if (a?.toLowerCase() !== "me") return a;
    this.me ??= (await this.request<{ username: string }>("GET", "/user")).username;
    return this.me;
  }

  /** In words, which issues Mako looks for; shown when a search finds nothing. */
  async describeFilter(): Promise<string> {
    const who = await this.assignee();
    return [
      "відкриті задачі",
      this.labels.trigger ? `з міткою «${this.labels.trigger}»` : "без вимоги мітки",
      who ? `призначені на ${who}` : "призначені на будь-кого",
      `у проєктах ${Object.keys(this.projects).join(", ")}`,
    ].join(" ");
  }

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `${this.issuePath(task)}/notes`, { body: text });
  }

  statusFor(moment: "start" | Outcome | "merged"): string | null | undefined {
    const t = this.cfg.transitions;
    if (moment === "merged") return t?.onMerged;
    return moment === "start" ? t?.onStart : moment === "success" ? t?.onSuccess : moment === "failed" ? t?.onFailure : undefined;
  }

  private mrPath(reviewUrl: string): string {
    const m = reviewUrl.match(/^https?:\/\/[^/]+\/(.+?)\/-\/merge_requests\/(\d+)/);
    if (!m) throw new Error(`не схоже на MR GitLab: ${reviewUrl}`);
    return `${projectApi(m[1])}/merge_requests/${m[2]}`;
  }

  async reviewDetails(reviewUrl: string): Promise<ReviewInfo> {
    const path = this.mrPath(reviewUrl);
    const mr = await this.request<any>("GET", path);
    const state = mr.state === "merged" ? "merged" : mr.state === "closed" ? "closed" : "open";
    let pipeline: ReviewInfo["pipeline"];
    if (mr.head_pipeline) {
      const all = (await this.request<any[]>("GET", `/projects/${mr.project_id}/pipelines/${mr.head_pipeline.id}/jobs?per_page=100`).catch(() => []))
        .sort((a, b) => a.id - b.id);
      const failed = all.filter((j) => j.status === "failed" && !j.allow_failure).map((j) => ({ name: j.name, url: j.web_url, id: j.id }));
      const jobs = all.map((j) => ({ name: j.name, stage: j.stage, status: j.status, url: j.web_url, allowFailure: Boolean(j.allow_failure) || undefined }));
      pipeline = { status: mr.head_pipeline.status, url: mr.head_pipeline.web_url, failed, jobs };
    }
    const discussions = state === "open" ? await this.request<any[]>("GET", `${path}/discussions?per_page=100`).catch(() => []) : [];
    const comments = discussions
      .filter((d) => d.notes?.[0] && !d.notes[0].system && d.notes[0].resolvable && !d.notes[0].resolved)
      .map((d) => {
        const n = d.notes[0];
        return {
          author: n.author?.name ?? n.author?.username ?? "reviewer",
          body: n.body,
          path: n.position?.new_path ?? n.position?.old_path,
          line: n.position?.new_line ?? n.position?.old_line ?? undefined,
          url: `${mr.web_url}#note_${n.id}`,
          replies: d.notes.length - 1,
          thread: String(d.id),
        };
      });
    const status: string = mr.detailed_merge_status ?? mr.merge_status ?? "";
    return {
      state,
      conflicts: Boolean(mr.has_conflicts),
      mergeable: state === "open" && status === "mergeable",
      blocker: state === "open" && status !== "mergeable" ? status : undefined,
      pipeline,
      comments,
      checkedAt: Date.now(),
    };
  }

  async replyToThread(reviewUrl: string, thread: string, body: string, resolve: boolean): Promise<void> {
    const path = `${this.mrPath(reviewUrl)}/discussions/${encodeURIComponent(thread)}`;
    await this.request("POST", `${path}/notes`, { body });
    if (resolve) await this.request("PUT", path, { resolved: true });
  }

  async mergeReview(reviewUrl: string): Promise<void> {
    await this.request("PUT", `${this.mrPath(reviewUrl)}/merge`, { should_remove_source_branch: true }).catch((err: any) => {
      throw new Error(`GitLab не змерджив MR: ${String(err.message).split("\n")[0]}`);
    });
  }

  async jobLog(reviewUrl: string, jobId: number): Promise<string> {
    const project = reviewUrl.match(/^https?:\/\/[^/]+\/(.+?)\/-\/merge_requests\//)?.[1] ?? "";
    const text = await requestText(`${this.apiUrl}${projectApi(project)}/jobs/${jobId}/trace`, this.headers);
    // eslint-disable-next-line no-control-regex
    return text.replace(/\x1b\[[0-9;]*[A-Za-z]|section_(start|end):\d+:\S+\r?/g, "").slice(-6000);
  }

  async reviewState(reviewUrl: string): Promise<"open" | "merged" | "closed"> {
    const m = reviewUrl.match(/^https?:\/\/[^/]+\/(.+?)\/-\/merge_requests\/(\d+)/);
    if (!m) throw new Error(`не схоже на MR GitLab: ${reviewUrl}`);
    const mr = await this.request<{ state: string }>("GET", `${projectApi(m[1])}/merge_requests/${m[2]}`);
    return mr.state === "merged" ? "merged" : mr.state === "closed" ? "closed" : "open";
  }

  /**
   * «Closes #N» closes the issue on merge, but its status may stay «In progress»: set the done status
   * (configured, or the project's own «Done») and close it if GitLab has not yet.
   */
  async onMerged(task: Task): Promise<string | void> {
    const configured = this.cfg.transitions?.onMerged;
    const name = configured === undefined ? await this.guessStatus(/^done$|^closed$|completed|готово|виконано/i, "done") : configured;
    const moved = await this.setStatus(task, name);
    const issue = await this.request<{ state: string }>("GET", this.issuePath(task));
    if (issue.state !== "closed") await this.request("PUT", this.issuePath(task), { state_event: "close" });
    return moved;
  }

  async onStart(task: Task): Promise<string | void> {
    // Not set at all (not even "не змінювати"): the project's own «In progress», when it has one.
    const configured = this.cfg.transitions?.onStart;
    const name = configured === undefined ? await this.guessStatus(/^in progress$|progress|в роботі|doing/i, "in progress") : configured;
    const moved = await this.setStatus(task, name);
    await this.applyLabels(task, labelChanges(this.labels, "start"));
    return moved;
  }

  async onFinish(task: Task, outcome: Outcome): Promise<string | void> {
    const t = this.cfg.transitions;
    const moved = await this.setStatus(task, outcome === "success" ? t?.onSuccess : outcome === "failed" ? t?.onFailure : undefined);
    await this.applyLabels(task, labelChanges(this.labels, outcome));
    return moved;
  }

  private statuses?: Promise<string[]>;

  /** A status of this GitLab by pattern; none when statuses are not available here (Free tier, older GitLab). */
  private async guessStatus(pattern: RegExp, exact = "in progress"): Promise<string | undefined> {
    this.statuses ??= this.listStatuses().catch(() => []);
    const all = await this.statuses;
    return all.find((s) => s.toLowerCase() === exact) ?? all.find((s) => pattern.test(s));
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
  private async setStatus(task: Task, name: string | null | undefined): Promise<string | undefined> {
    if (!name) return undefined;
    // Tasks taken before Mako kept the issue's global id: look it up.
    const id = task.nativeId ?? String((await this.request<any>("GET", this.issuePath(task))).id);
    const data = await this.graphql<any>(
      `mutation($id: WorkItemID!, $name: String!) { workItemUpdate(input: { id: $id, statusWidget: { name: $name } }) { errors } }`,
      { id: `gid://gitlab/WorkItem/${id}`, name },
    );
    const errors: string[] = data?.workItemUpdate?.errors ?? [];
    if (errors.length) throw new Error(`GitLab не змінив статус ${task.id} на «${name}»: ${errors.join("; ")}`);
    return name;
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

  async describeReview(reviewUrl: string, title: string, description: string): Promise<void> {
    const m = reviewUrl.match(/^https?:\/\/[^/]+\/(.+?)\/-\/merge_requests\/(\d+)/);
    if (!m) throw new Error(`не схоже на MR GitLab: ${reviewUrl}`);
    await this.request("PUT", `${projectApi(m[1])}/merge_requests/${m[2]}`, { title, description });
  }

  async listProjects(): Promise<{ key: string; name: string }[]> {
    const list = await this.request<{ path_with_namespace: string; name_with_namespace: string }[]>(
      "GET",
      "/projects?membership=true&simple=true&archived=false&order_by=last_activity_at&per_page=100",
    );
    return list.map((p) => ({ key: p.path_with_namespace, name: p.name_with_namespace }));
  }

  private user?: Promise<{ id: number; username: string }>;

  async whoAmI(): Promise<string> {
    this.user ??= this.request<{ id: number; username: string }>("GET", "/user");
    return (await this.user).username;
  }

  async assignToMe(task: Task): Promise<void> {
    this.user ??= this.request<{ id: number; username: string }>("GET", "/user");
    await this.request("PUT", this.issuePath(task), { assignee_ids: [(await this.user).id] });
  }

  async issueState(task: Task): Promise<IssueState> {
    try {
      const issue = await requestJson<any>("GET", `${this.apiUrl}${this.issuePath(task)}`, this.headers);
      return issue.state === "closed" ? "closed" : "open";
    } catch (err) {
      if (err instanceof HttpError && (err.status === 404)) return "deleted";
      throw err;
    }
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
