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
  /** Who the issue is assigned to: usernames (GitLab), logins (GitHub) or account ids (Jira). */
  assignees?: string[];
}

export type Outcome = "success" | "no-changes" | "failed";

/** A PR/MR as the developer needs it next to the agent: CI, open review threads, conflicts, whether it can be merged. */
export interface ReviewInfo {
  state: "open" | "merged" | "closed";
  conflicts: boolean;
  /** Ready to merge by the host's own rules (CI, approvals, threads). */
  mergeable: boolean;
  /** Why not, in the host's words ("ci_still_running", "not_approved", …), when it is not. */
  blocker?: string;
  pipeline?: {
    status: string;
    url?: string;
    failed: { name: string; url?: string; id?: number }[];
    /** Every job of the pipeline in run order, latest try only. */
    jobs?: { name: string; stage?: string; status: string; url?: string; allowFailure?: boolean }[];
  };
  /** Unresolved review threads, first comment of each. */
  comments: { author: string; body: string; path?: string; line?: number; url?: string; replies: number }[];
  checkedAt: number;
}

/** Where an issue stands in its tracker, for tasks that left the search results. */
export type IssueState = "open" | "closed" | "deleted";

export interface Tracker {
  id: string;
  /** Display name: "Jira", "GitHub", "GitLab". */
  name: string;
  projects: Record<string, ProjectConfig>;
  fetchTasks(): Promise<Task[]>;
  comment(task: Task, text: string): Promise<void>;
  /** Both return the status the issue was moved to, if any. */
  onStart(task: Task): Promise<string | void>;
  onFinish(task: Task, outcome: Outcome): Promise<string | void>;
  /** Statuses an issue can be moved to, for the settings page; undefined when the tracker has none. */
  listStatuses?(): Promise<string[]>;
  /** Which issues the tracker looks for, in words, for an empty search result. */
  describeFilter?(): Promise<string>;
  /** Whether the issue is still open, closed or deleted. */
  issueState?(task: Task): Promise<IssueState>;
  /** The token owner, in the form of Task.assignees. */
  whoAmI?(): Promise<string>;
  /** Assigns the issue to the token owner, so colleagues see it is taken. */
  assignToMe?(task: Task): Promise<void>;
  /** For trackers with statuses: which one a moment moves the issue to; empty when none is set. */
  statusFor?(moment: "start" | Outcome | "merged"): string | null | undefined;
  /** Whether a PR/MR opened for one of its issues is still open, merged or closed without merging. */
  reviewState?(reviewUrl: string): Promise<"open" | "merged" | "closed">;
  /** The PR/MR was merged: move the issue to its final status and close it. Returns the status set, if any. */
  onMerged?(task: Task): Promise<string | void>;
  /** CI, review threads, conflicts and mergeability of a PR/MR. */
  reviewDetails?(reviewUrl: string): Promise<ReviewInfo>;
  /** Merges the PR/MR; throws with the host's reason when it cannot. */
  mergeReview?(reviewUrl: string): Promise<void>;
  /** The end of a failed CI job's log, for the agent to fix it. */
  jobLog?(reviewUrl: string, jobId: number): Promise<string>;
  /** Sets the title and full (multi-line) description of a PR/MR opened for one of its issues. */
  describeReview?(reviewUrl: string, title: string, description: string): Promise<void>;
}
