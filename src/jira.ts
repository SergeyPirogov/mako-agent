import type { Config } from "./config.js";

export interface JiraIssue {
  key: string;
  projectKey: string;
  summary: string;
  description: string;
  issueType: string;
  priority: string;
  url: string;
  comments: { author: string; body: string }[];
}

export class JiraClient {
  private auth: string;

  constructor(private cfg: Config["jira"]) {
    this.auth = "Basic " + Buffer.from(`${cfg.email}:${cfg.apiToken}`).toString("base64");
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await fetch(`${this.cfg.baseUrl}${path}`, {
      method,
      headers: { Authorization: this.auth, Accept: "application/json", "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`Jira ${method} ${path} -> ${res.status}: ${await res.text()}`);
    return (res.status === 204 ? undefined : await res.json()) as T;
  }

  async search(jql: string, maxResults = 50): Promise<JiraIssue[]> {
    const data = await this.request<{ issues: any[] }>("POST", "/rest/api/3/search/jql", {
      jql,
      maxResults,
      fields: ["summary", "description", "issuetype", "priority", "project", "comment"],
    });
    return data.issues.map((i) => ({
      key: i.key,
      projectKey: i.fields.project.key,
      summary: i.fields.summary ?? "",
      description: adfToText(i.fields.description),
      issueType: i.fields.issuetype?.name ?? "",
      priority: i.fields.priority?.name ?? "",
      url: `${this.cfg.baseUrl}/browse/${i.key}`,
      comments: (i.fields.comment?.comments ?? []).map((c: any) => ({
        author: c.author?.displayName ?? "unknown",
        body: adfToText(c.body),
      })),
    }));
  }

  async comment(key: string, text: string): Promise<void> {
    await this.request("POST", `/rest/api/3/issue/${key}/comment`, { body: textToAdf(text) });
  }

  /** Moves the issue to the status whose transition (or target status) name matches, case-insensitively. */
  async transition(key: string, name: string): Promise<boolean> {
    const { transitions } = await this.request<{ transitions: any[] }>("GET", `/rest/api/3/issue/${key}/transitions`);
    const wanted = name.toLowerCase();
    const t = transitions.find((t) => t.name.toLowerCase() === wanted || t.to?.name?.toLowerCase() === wanted);
    if (!t) return false;
    await this.request("POST", `/rest/api/3/issue/${key}/transitions`, { transition: { id: t.id } });
    return true;
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
