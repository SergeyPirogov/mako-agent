import { readFileSync, existsSync } from "node:fs";
import path from "node:path";
import type { TrackerConfig } from "./trackers/index.js";

export interface RepoConfig {
  /** Name used by `repo:<name>` labels and `default`. */
  name: string;
  repoPath: string;
  baseBranch: string;
  worktreesDir: string;
  /** Where to open the pull/merge request; "auto" picks by the origin remote URL. */
  pullRequest: "auto" | "github" | "gitlab" | "none";
  extraInstructions?: string;
  /** The task goes to this repo when it has any of these labels or (Jira) components. */
  match?: { labels?: string[]; components?: string[] };
}

/** One tracker project (Jira project, GitHub/GitLab repo) and the local repositories its tasks can go to. */
export interface ProjectConfig {
  repos: Record<string, RepoConfig>;
  /** Repo used when no label or rule matches; with a single repo it is the default implicitly. */
  default?: string;
}

export interface NotificationConfig {
  enabled: boolean;
  /** Skip the question and start every new task right away. */
  autoTake: boolean;
  /** How long "Відкласти" (or an ignored notification) postpones the task. */
  snoozeMin: number;
  /** How long a notification waits for an answer before counting as ignored. */
  timeoutSec: number;
  /** Port of the local inbox page; null disables it. */
  inboxPort: number | null;
}

export interface Config {
  pollIntervalSec: number;
  maxConcurrent: number;
  taskTimeoutMin: number;
  stateDir: string;
  notifications: NotificationConfig;
  trackers: TrackerConfig[];
  claude: { command: string; args: string[] };
  dryRun: boolean;
}

export function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Missing env variable ${name} (see .env.example)`);
  return value;
}

export function loadConfig(configPath: string, dryRun: boolean): Config {
  if (existsSync(".env")) process.loadEnvFile(".env");
  if (!existsSync(configPath)) {
    throw new Error(`Config not found: ${configPath}. Copy agent.config.example.json to agent.config.json`);
  }
  const raw = JSON.parse(readFileSync(configPath, "utf8"));

  const trackers: TrackerConfig[] = (raw.trackers ?? []).map((t: any) => {
    const projects: Record<string, ProjectConfig> = {};
    for (const [key, p] of Object.entries<any>(t.projects ?? {})) {
      // Short form: the project itself is a single repo.
      const rawRepos: Record<string, any> = p.repos ?? { [key.split("/").pop()!]: p };
      const repos: Record<string, RepoConfig> = {};
      for (const [name, r] of Object.entries<any>(rawRepos)) {
        if (!r.repoPath) throw new Error(`Project ${key}, repo ${name}: repoPath is required`);
        repos[name] = {
          name,
          repoPath: path.resolve(r.repoPath),
          baseBranch: r.baseBranch ?? "main",
          worktreesDir: path.resolve(r.worktreesDir ?? path.join(r.repoPath, "..", ".worktrees", name)),
          pullRequest: r.pullRequest ?? "auto",
          extraInstructions: r.extraInstructions,
          match: r.match,
        };
      }
      if (p.default && !repos[p.default]) throw new Error(`Project ${key}: default repo "${p.default}" is not in repos`);
      projects[key] = { repos, default: p.default };
    }
    if (!Object.keys(projects).length) throw new Error(`Tracker ${t.id ?? t.type} has no projects`);
    return { ...t, projects };
  });
  if (!trackers.length) throw new Error("Config has no trackers");

  const n = raw.notifications ?? {};
  return {
    pollIntervalSec: raw.pollIntervalSec ?? 60,
    maxConcurrent: raw.maxConcurrent ?? 1,
    taskTimeoutMin: raw.taskTimeoutMin ?? 45,
    stateDir: path.resolve(raw.stateDir ?? ".agent-state"),
    notifications: {
      enabled: n.enabled ?? true,
      autoTake: n.autoTake ?? false,
      snoozeMin: n.snoozeMin ?? 60,
      timeoutSec: n.timeoutSec ?? 600,
      inboxPort: n.inboxPort === undefined ? 7777 : n.inboxPort,
    },
    trackers,
    claude: { command: raw.claude?.command ?? "claude", args: raw.claude?.args ?? [] },
    dryRun,
  };
}
