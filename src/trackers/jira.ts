import type { ProjectConfig } from "../config.js";
import { requestJson, requestFile, explainAccess, HttpError } from "../http.js";
import type { Attachment, IssueState, Outcome, Task, Tracker } from "../types.js";

export interface JiraTrackerConfig {
  type: "jira";
  id?: string;
  /** Defaults to env JIRA_BASE_URL. Credentials come from env JIRA_EMAIL and JIRA_API_TOKEN. */
  baseUrl?: string;
  jql: string;
  /** Status (or transition) names; null leaves the status unchanged. */
  transitions?: { onStart?: string | null; onSuccess?: string | null; onFailure?: string | null; onMerged?: string | null };
  projects: Record<string, ProjectConfig>;
}

export class JiraTracker implements Tracker {
  readonly name = "Jira";
  readonly id: string;
  readonly projects: Record<string, ProjectConfig>;
  private baseUrl: string;
  private headers: Record<string, string>;

  constructor(private cfg: JiraTrackerConfig, env: (name: string) => string) {
    this.id = cfg.id ?? "jira";
    this.projects = cfg.projects;
    this.baseUrl = (cfg.baseUrl ?? env("JIRA_BASE_URL")).replace(/\/$/, "");
    const auth = Buffer.from(`${env("JIRA_EMAIL")}:${env("JIRA_API_TOKEN")}`).toString("base64");
    this.headers = { Authorization: `Basic ${auth}` };
  }

  private account?: Promise<string>;

  async whoAmI(): Promise<string> {
    this.account ??= this.request<{ accountId: string }>("GET", "/rest/api/3/myself").then((u) => u.accountId);
    return this.account;
  }

  async assignToMe(task: Task): Promise<void> {
    await this.request("PUT", `/rest/api/3/issue/${encodeURIComponent(task.id)}/assignee`, { accountId: await this.whoAmI() });
  }

  async issueState(task: Task): Promise<IssueState> {
    try {
      const issue = await requestJson<any>("GET", `${this.baseUrl}/rest/api/3/issue/${encodeURIComponent(task.id)}?fields=status`, this.headers);
      return issue.fields?.status?.statusCategory?.key === "done" ? "closed" : "open";
    } catch (err) {
      if (err instanceof HttpError && err.status === 404) return "deleted";
      throw err;
    }
  }

  private request<T>(method: string, path: string, body?: unknown): Promise<T> {
    return requestJson<T>(method, `${this.baseUrl}${path}`, this.headers, body).catch((err) =>
      explainAccess(err, (status) => {
        if (status === 401) return "Jira не прийняла дані входу (401): перевірте JIRA_EMAIL і API-токен (id.atlassian.com → Security → API tokens)";
        if (status === 403) return "Jira заборонила дію (403): у вашого акаунта немає потрібних прав у цьому проєкті";
        return undefined;
      }),
    );
  }

  async fetchTasks(): Promise<Task[]> {
    return this.search(this.cfg.jql, 50);
  }

  async browseTasks(): Promise<Task[]> {
    const keys = Object.keys(this.projects).map((k) => `"${k.replace(/"/g, "")}"`);
    return this.search(`project in (${keys.join(", ")}) AND statusCategory != Done ORDER BY updated DESC`, 100);
  }

  async inspectTask(ref: string): Promise<{ task: Task; misses: string[] } | undefined> {
    const key = (ref.match(/\/browse\/([A-Z][A-Z0-9_]*-\d+)/) ?? ref.match(/^([A-Z][A-Z0-9_]*-\d+)$/i))?.[1]?.toUpperCase();
    if (!key) return undefined;
    const [task] = await this.search(`key = ${key}`, 1);
    if (!task) throw new Error(`${key}: задачі немає або токен її не бачить`);
    const misses: string[] = [];
    if (!this.projects[task.project]) misses.push(`проєкт ${task.project} не доданий у Налаштуваннях`);
    // The JQL can say anything: ask Jira whether this issue is in it.
    const inQuery = await this.search(`(${this.cfg.jql.replace(/\s+ORDER\s+BY[\s\S]*$/i, "")}) AND key = ${key}`, 1);
    if (!inQuery.length) misses.push(`не підходить під JQL: ${this.cfg.jql}`);
    return { task, misses };
  }

  private async search(jql: string, maxResults: number): Promise<Task[]> {
    const data = await this.request<{ issues: any[] }>("POST", "/rest/api/3/search/jql", {
      jql,
      maxResults,
      fields: ["summary", "description", "issuetype", "priority", "project", "comment", "labels", "components", "assignee", "attachment"],
    });
    return data.issues.map((i) => ({
      id: i.key,
      slug: i.key,
      trackerId: this.id,
      project: i.fields.project.key,
      title: i.fields.summary ?? "",
      description: adfToText(i.fields.description).trim(),
      url: `${this.baseUrl}/browse/${i.key}`,
      meta: `Type: ${i.fields.issuetype?.name ?? "?"} | Priority: ${i.fields.priority?.name ?? "?"}`,
      labels: i.fields.labels ?? [],
      components: (i.fields.components ?? []).map((c: any) => c.name),
      comments: (i.fields.comment?.comments ?? []).map((c: any) => ({
        author: c.author?.displayName ?? "unknown",
        body: adfToText(c.body).trim(),
      })),
      closingRef: i.key,
      assignees: i.fields.assignee?.accountId ? [i.fields.assignee.accountId] : [],
      assigneeNames: i.fields.assignee?.displayName ? [i.fields.assignee.displayName] : undefined,
      attachments: (i.fields.attachment ?? [])
        .filter((a: any) => a?.content)
        .map((a: any) => ({ name: a.filename ?? "file", url: a.content, where: "вкладення задачі" })),
    }));
  }

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `/rest/api/3/issue/${task.id}/comment`, { body: textToAdf(text) });
  }

  statusFor(moment: "start" | Outcome | "merged"): string | null | undefined {
    const t = this.cfg.transitions;
    if (moment === "merged") return t?.onMerged;
    return moment === "start" ? t?.onStart : moment === "success" ? t?.onSuccess : moment === "failed" ? t?.onFailure : undefined;
  }

  async listProjects(): Promise<{ key: string; name: string }[]> {
    const page = await this.request<{ values: { key: string; name: string }[] }>("GET", "/rest/api/3/project/search?maxResults=100&orderBy=-lastIssueUpdatedTime");
    return page.values.map((p) => ({ key: p.key, name: `${p.key} — ${p.name}` }));
  }

  async downloadAttachment(a: Attachment, maxBytes: number): Promise<Buffer> {
    return requestFile(a.url, this.headers, maxBytes);
  }

  /** Statuses of the configured projects' workflows, for the settings page to suggest. */
  async listStatuses(): Promise<string[]> {
    const names = new Set<string>();
    for (const key of Object.keys(this.projects)) {
      const types = await this.request<{ statuses?: { name: string }[] }[]>("GET", `/rest/api/3/project/${encodeURIComponent(key)}/statuses`);
      for (const t of types) for (const s of t.statuses ?? []) names.add(s.name);
    }
    return [...names];
  }

  async onMerged(task: Task): Promise<string | void> {
    const configured = this.cfg.transitions?.onMerged;
    if (configured !== undefined) return this.transition(task.id, configured);
    // Not set: «Done» when the workflow has it; Jira projects differ, so no transition is not an error.
    return this.transition(task.id, "Done").catch(() => undefined);
  }

  async onStart(task: Task): Promise<string | void> {
    return this.transition(task.id, this.cfg.transitions?.onStart);
  }

  async onFinish(task: Task, outcome: Outcome): Promise<string | void> {
    const t = this.cfg.transitions;
    if (outcome === "success") return this.transition(task.id, t?.onSuccess);
    if (outcome === "failed") return this.transition(task.id, t?.onFailure);
  }

  /** Moves the issue to the status whose transition (or target status) name matches, case-insensitively. */
  private async transition(key: string, name: string | null | undefined): Promise<string | undefined> {
    if (!name) return undefined;
    const { transitions } = await this.request<{ transitions: any[] }>("GET", `/rest/api/3/issue/${key}/transitions`);
    const wanted = name.toLowerCase();
    const t = transitions.find((t) => t.name.toLowerCase() === wanted || t.to?.name?.toLowerCase() === wanted);
    if (!t) throw new Error(`${key}: у Jira немає переходу в «${name}» з поточного статусу`);
    await this.request("POST", `/rest/api/3/issue/${key}/transitions`, { transition: { id: t.id } });
    return name;
  }
}

/** Flattens Atlassian Document Format into plain text good enough for a prompt. */
export function adfToText(node: any): string {
  if (!node) return "";
  if (typeof node === "string") return node;
  switch (node.type) {
    case "text":
      return node.text ?? "";
    case "hardBreak":
      return "\n";
    case "mention":
      return node.attrs?.text ?? "@user";
    case "inlineCard":
      return node.attrs?.url ?? "";
    case "codeBlock":
      return "```\n" + (node.content ?? []).map(adfToText).join("") + "\n```\n\n";
    case "listItem":
      return "- " + (node.content ?? []).map(adfToText).join("").trim() + "\n";
    case "heading":
      return "#".repeat(node.attrs?.level ?? 1) + " " + (node.content ?? []).map(adfToText).join("") + "\n\n";
    case "paragraph":
      return (node.content ?? []).map(adfToText).join("") + "\n\n";
    default:
      return (node.content ?? []).map(adfToText).join("");
  }
}

function textToAdf(text: string) {
  return {
    type: "doc",
    version: 1,
    content: text.split(/\n{2,}/).map((para) => ({
      type: "paragraph",
      content: para.split("\n").flatMap((line, i) => [
        ...(i > 0 ? [{ type: "hardBreak" }] : []),
        ...(line ? [{ type: "text", text: line }] : []),
      ]),
    })),
  };
}
