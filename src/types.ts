import type { ProjectConfig } from "./config.js";

/** A unit of work fetched from any tracker, normalized for the orchestrator and the prompt. */
export interface Task {
  /** Human-readable unique id: "PROJ-12", "acme/web#42", "group/app#7". */
  id: string;
  /** Filesystem- and branch-safe form of the id: "PROJ-12", "web-42". */
  slug: string;
  trackerId: string;
  /** Key of the project in the tracker config that maps the task to a local repository. */
  project: string;
  title: string;
  description: string;
  url: string;
  /** Extra context line for the prompt, e.g. "Type: Bug | Priority: High". */
  meta?: string;
  labels: string[];
  /** Jira components; empty for GitHub/GitLab. */
  components: string[];
  comments: { author: string; body: string }[];
  /** Text that closes the issue when put into a PR/MR description, e.g. "Closes acme/web#42". */
  closingRef?: string;
  /** The tracker's own id of the issue when it differs from `id` (GitLab's global issue id for GraphQL). */
  nativeId?: string;
}

export type Outcome = "success" | "no-changes" | "failed";

export interface Tracker {
  id: string;
  /** Display name: "Jira", "GitHub", "GitLab". */
  name: string;
  projects: Record<string, ProjectConfig>;
  fetchTasks(): Promise<Task[]>;
  comment(task: Task, text: string): Promise<void>;
  onStart(task: Task): Promise<void>;
  onFinish(task: Task, outcome: Outcome): Promise<void>;
  /** Statuses an issue can be moved to, for the settings page; undefined when the tracker has none. */
  listStatuses?(): Promise<string[]>;
}
