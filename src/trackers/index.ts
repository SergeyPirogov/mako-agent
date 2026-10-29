import type { Tracker } from "../types.js";
import { JiraTracker, type JiraTrackerConfig } from "./jira.js";
import { GitHubTracker, type GitHubTrackerConfig } from "./github.js";
import { GitLabTracker, type GitLabTrackerConfig } from "./gitlab.js";

export type TrackerConfig = JiraTrackerConfig | GitHubTrackerConfig | GitLabTrackerConfig;

export function createTracker(cfg: TrackerConfig, env: (name: string) => string): Tracker {
  switch (cfg.type) {
    case "jira":
      return new JiraTracker(cfg, env);
    case "github":
      return new GitHubTracker(cfg);
    case "gitlab":
      return new GitLabTracker(cfg, env);
    default:
      throw new Error(`Unknown tracker type: ${(cfg as any).type}. Use jira, github or gitlab`);
  }
}
