import type { ProjectConfig } from "../config.js";
import type { IssueState, Task, Tracker } from "../types.js";

export const LOCAL_TRACKER = "mako";

/**
 * Tasks typed into Mako itself («+ Задача»), with no issue anywhere: every configured repo is a project
 * (keyed by repo name), and there is nobody to comment to or move statuses for.
 */
export class LocalTracker implements Tracker {
  readonly id = LOCAL_TRACKER;
  readonly name = "Mako";
  readonly projects: Record<string, ProjectConfig>;

  constructor(trackers: Tracker[]) {
    this.projects = {};
    for (const t of trackers)
      for (const p of Object.values(t.projects))
        for (const r of Object.values(p.repos)) this.projects[r.name] ??= { repos: { [r.name]: r } };
  }

  async fetchTasks(): Promise<Task[]> {
    return [];
  }
  async comment(): Promise<void> {}
  async onStart(): Promise<void> {}
  async onFinish(): Promise<void> {}
  async issueState(): Promise<IssueState> {
    return "open";
  }
}

/** A task the developer wrote in Mako; ids go mako#1, mako#2, … */
export function localTask(n: number, title: string, description: string, repo: string): Task {
  return {
    id: `${LOCAL_TRACKER}#${n}`,
    slug: `${repo}-m${n}`,
    trackerId: LOCAL_TRACKER,
    project: repo,
    title,
    description,
    url: "",
    labels: [],
    components: [],
    comments: [],
  };
}
