import type { ProjectConfig } from "../config.js";
import { requestJson, explainAccess, HttpError } from "../http.js";
import type { IssueState, Outcome, Task, Tracker } from "../types.js";

export interface JiraTrackerConfig {
  type: "jira";
  id?: string;
  /** Defaults to env JIRA_BASE_URL. Credentials come from env JIRA_EMAIL and JIRA_API_TOKEN. */
  baseUrl?: string;
  jql: string;
  /** Status (or transition) names; null leaves the status unchanged. */
  transitions?: { onStart?: string | null; onSuccess?: string | null; onFailure?: string | null };
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
    const data = await this.request<{ issues: any[] }>("POST", "/rest/api/3/search/jql", {
      jql: this.cfg.jql,
      maxResults: 50,
      fields: ["summary", "description", "issuetype", "priority", "project", "comment", "labels", "components"],
    });
    return data.issues.map((i) => ({
      id: i.key,
      slug: i.key,
      trackerId: this.id,
      project: i.fields.project.key,
      title: i.fields.summary ?? "",
      description: adfToText(i.fields.description),
      url: `${this.baseUrl}/browse/${i.key}`,
      meta: `Type: ${i.fields.issuetype?.name ?? "?"} | Priority: ${i.fields.priority?.name ?? "?"}`,
      labels: i.fields.labels ?? [],
      components: (i.fields.components ?? []).map((c: any) => c.name),
      comments: (i.fields.comment?.comments ?? []).map((c: any) => ({
        author: c.author?.displayName ?? "unknown",
        body: adfToText(c.body),
      })),
      closingRef: i.key,
    }));
  }

  async comment(task: Task, text: string): Promise<void> {
    await this.request("POST", `/rest/api/3/issue/${task.id}/comment`, { body: textToAdf(text) });
  }

  statusFor(moment: "start" | Outcome): string | null | undefined {
    const t = this.cfg.transitions;
    return moment === "start" ? t?.onStart : moment === "success" ? t?.onSuccess : moment === "failed" ? t?.onFailure : undefined;
  }

  async onStart(task: Task): Promise<void> {
    await this.transition(task.id, this.cfg.transitions?.onStart);
  }

  async onFinish(task: Task, outcome: Outcome): Promise<void> {
    const t = this.cfg.transitions;
    if (outcome === "success") await this.transition(task.id, t?.onSuccess);
    if (outcome === "failed") await this.transition(task.id, t?.onFailure);
  }

  /** Moves the issue to the status whose transition (or target status) name matches, case-insensitively. */
  private async transition(key: string, name: string | null | undefined): Promise<void> {
    if (!name) return;
    const { transitions } = await this.request<{ transitions: any[] }>("GET", `/rest/api/3/issue/${key}/transitions`);
    const wanted = name.toLowerCase();
    const t = transitions.find((t) => t.name.toLowerCase() === wanted || t.to?.name?.toLowerCase() === wanted);
    if (!t) throw new Error(`${key}: no transition to "${name}" available`);
    await this.request("POST", `/rest/api/3/issue/${key}/transitions`, { transition: { id: t.id } });
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
