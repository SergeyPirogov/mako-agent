import { readFileSync, existsSync, writeFileSync, renameSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import type { TrackerConfig } from "./trackers/index.js";
import type { ExecutorConfig } from "./executors/index.js";

export interface RepoConfig {
  /** Name used by `repo:<name>` labels and `default`. */
  name: string;
  repoPath: string;
  baseBranch: string;
  worktreesDir: string;
  /** Where to open the pull/merge request; "auto" picks by the origin remote URL. */
  pullRequest: "auto" | "github" | "gitlab" | "none";
  extraInstructions?: string;
  /** Name from `executors`; defaults to `defaultExecutor`. */
  executor?: string;
  /**
   * Prepares a fresh worktree before the agent starts (installs dependencies, so tests, linters and git hooks work).
   * "auto" picks the command from the lock file; "none" skips it.
   */
  setup: string;
  /** The task goes to this repo when it has any of these labels or (Jira) components. */
  match?: { labels?: string[]; components?: string[] };
  /** Name of a task's branch: {type} (feat/fix) {id} {title} {slug}; this repo's own, or the general one. */
  branchPattern: string;
}

/** feat/12-add-dark-mode, fix/34-login-button: the usual names, with the type taken from the issue. */
export const DEFAULT_BRANCH_PATTERN = "{type}/{id}-{title}";

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
  /** Repeat the notification while an agent keeps waiting for you; 0 = notify once. */
  remindMin: number;
}

export interface Config {
  pollIntervalSec: number;
  /** Kanban WIP limit: at most this many tasks taken and unfinished; trackers are not polled while full. 0 = no limit. */
  wipLimit: number;
  maxConcurrent: number;
  taskTimeoutMin: number;
  stateDir: string;
  notifications: NotificationConfig;
  trackers: TrackerConfig[];
  /** How often to look for a PR/MR on handed-off tasks; 0 turns it off. */
  handoffCheckMin: number;
  /** Named ways to run an agent; repos pick one with `executor`. */
  executors: Record<string, ExecutorConfig>;
  defaultExecutor: string;
  dryRun: boolean;
  /** Every taken task starts with a plan the developer approves (default); a task can opt out when taken. */
  planFirst: boolean;
}

/** Absolute path, with a leading ~/ meaning the home folder (as people type it in the settings page). */
export function resolvePath(p: string): string {
  return path.resolve(p.startsWith("~/") ? path.join(os.homedir(), p.slice(2)) : p);
}

export function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Missing env variable ${name} (see .env.example)`);
  return value;
}

export function loadConfig(configPath: string, dryRun: boolean): Config {
  const raw = readRawConfig(configPath);
  if (!raw) throw new Error(`Config not found: ${configPath}. Run \`mako\` and open the settings page, or copy mako.config.example.json`);
  return parseConfig(raw, dryRun);
}

/** The config file as written by the user (or the settings page), or undefined when there is none yet. */
export function readRawConfig(configPath: string): any | undefined {
  if (!existsSync(configPath)) return undefined;
  return JSON.parse(readFileSync(configPath, "utf8"));
}

export function writeRawConfig(configPath: string, raw: unknown): void {
  const tmp = configPath + ".tmp";
  writeFileSync(tmp, JSON.stringify(raw, null, 2) + "\n");
  renameSync(tmp, configPath);
}

/** Validates a raw config and fills in defaults. Throws a readable error for the first problem found. */
export function parseConfig(raw: any, dryRun = false): Config {
  if (!raw || typeof raw !== "object") throw new Error("Config must be a JSON object");

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
          repoPath: resolvePath(r.repoPath),
          baseBranch: r.baseBranch ?? "main",
          worktreesDir: resolvePath(r.worktreesDir ?? path.join(resolvePath(r.repoPath), "..", ".worktrees", name)),
          pullRequest: r.pullRequest ?? "auto",
          extraInstructions: r.extraInstructions,
          match: r.match,
          executor: r.executor,
          setup: typeof r.setup === "string" && r.setup.trim() ? r.setup.trim() : "auto",
          branchPattern: (r.branchPattern || raw.branchPattern || DEFAULT_BRANCH_PATTERN).trim(),
        };
      }
      if (p.default && !repos[p.default]) throw new Error(`Project ${key}: default repo "${p.default}" is not in repos`);
      projects[key] = { repos, default: p.default };
    }
    if (!Object.keys(projects).length) throw new Error(`Tracker ${t.id ?? t.type} has no projects`);
    return { ...t, projects };
  });
  if (!trackers.length) throw new Error("Config has no trackers");

  // Older configs had a single `claude` block: that is the headless Claude executor.
  const executors: Record<string, ExecutorConfig> = raw.executors ?? {
    claude: { type: "claude-headless", command: raw.claude?.command, args: raw.claude?.args },
  };
  // Without a choice: Mako's own terminal, where the developer sees the agent and can step in.
  const defaultExecutor: string =
    raw.defaultExecutor ?? Object.keys(executors).find((n) => (executors as any)[n].type === "terminal") ?? Object.keys(executors)[0];
  if (!executors[defaultExecutor]) throw new Error(`defaultExecutor "${defaultExecutor}" is not in executors`);
  for (const t of trackers)
    for (const [key, p] of Object.entries(t.projects))
      for (const r of Object.values(p.repos))
        if (r.executor && !executors[r.executor])
          throw new Error(`Project ${key}, repo ${r.name}: executor "${r.executor}" is not in executors`);

  const n = raw.notifications ?? {};
  return {
    pollIntervalSec: raw.pollIntervalSec ?? 60,
    wipLimit: raw.wipLimit ?? 0,
    maxConcurrent: raw.maxConcurrent ?? (raw.wipLimit || 1),
    taskTimeoutMin: raw.taskTimeoutMin ?? 45,
    handoffCheckMin: raw.handoffCheckMin ?? 5,
    stateDir: path.resolve(raw.stateDir ?? ".mako"),
    notifications: {
      enabled: n.enabled ?? true,
      autoTake: n.autoTake ?? false,
      snoozeMin: n.snoozeMin ?? 60,
      timeoutSec: n.timeoutSec ?? 600,
      inboxPort: n.inboxPort === undefined ? 7777 : n.inboxPort,
      remindMin: n.remindMin ?? 5,
    },
    trackers,
    executors,
    defaultExecutor,
    dryRun,
    planFirst: raw.planFirst ?? true,
  };
}
