import { readFileSync, existsSync, writeFileSync, renameSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import type { TrackerConfig } from "./trackers/index.js";
import type { ExecutorConfig } from "./executors/index.js";

export interface RepoConfig {
  /** Name used by `repo:<name>` labels and `default`. */
  name: string;
  /** The local clone; by default `<projectsDir>/<name>`, cloned by Mako before the first task. */
  repoPath: string;
  /** Where Mako clones the repo from when `repoPath` has none: set, or the tracker's GitLab/GitHub project over SSH. */
  cloneUrl?: string;
  baseBranch: string;
  /** No base branch set: it is the remote's default (origin/HEAD), looked up again after a clone. */
  baseAuto: boolean;
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

export interface TaskTemplate {
  name: string;
  instructions: string;
  /** Applies to tasks with any of these labels… */
  labels: string[];
  /** …or whose title contains any of these words. */
  titleContains: string[];
}

/** feat/12-add-dark-mode, fix/34-login-button: the usual names, with the type taken from the issue. */
export const DEFAULT_BRANCH_PATTERN = "{type}/{id}-{title}";

/** Where projects without a path of their own are cloned. */
export const DEFAULT_PROJECTS_DIR = "~/mako/projects";

/**
 * The remote's default branch of a clone, from its files (no git call, no network): origin/HEAD, which a clone sets,
 * else whichever of main/master/develop the remote has. Undefined when there is no clone yet.
 */
export function detectBaseBranch(repoPath: string): string | undefined {
  const gitDir = path.join(repoPath, ".git");
  const read = (f: string) => {
    try {
      return readFileSync(path.join(gitDir, f), "utf8");
    } catch {
      return "";
    }
  };
  const head = read("refs/remotes/origin/HEAD").match(/^ref: refs\/remotes\/origin\/(.+)$/m)?.[1]?.trim();
  if (head) return head;
  const packed = read("packed-refs");
  return ["main", "master", "develop"].find((b) => existsSync(path.join(gitDir, "refs/remotes/origin", b)) || packed.includes(` refs/remotes/origin/${b}\n`));
}

/** One local repository as Mako uses it, from its settings (`r`) and the defaults. */
export function repoConfig(name: string, r: any, cloneUrl: string | undefined, projectsDir: string, branchPattern?: string): RepoConfig {
  const repoPath = r.repoPath ? resolvePath(r.repoPath) : path.join(projectsDir, name);
  return {
    name,
    repoPath,
    cloneUrl: r.cloneUrl || cloneUrl,
    baseBranch: r.baseBranch || detectBaseBranch(repoPath) || "main",
    baseAuto: !r.baseBranch,
    worktreesDir: resolvePath(r.worktreesDir ?? path.join(repoPath, "..", ".worktrees", name)),
    pullRequest: r.pullRequest ?? "auto",
    extraInstructions: r.extraInstructions,
    match: r.match,
    executor: r.executor,
    setup: typeof r.setup === "string" && r.setup.trim() ? r.setup.trim() : "auto",
    branchPattern: (r.branchPattern || branchPattern || DEFAULT_BRANCH_PATTERN).trim(),
  };
}

/** SSH address of a GitLab/GitHub tracker project ("group/app"), the way developers usually clone. */
export function defaultCloneUrl(tracker: any, projectKey: string): string | undefined {
  if (!projectKey.includes("/")) return undefined; // a Jira key says nothing about where the code is
  let host: string;
  try {
    if (tracker.type === "gitlab") host = new URL(tracker.baseUrl ?? "https://gitlab.com").hostname;
    else if (tracker.type === "github") host = tracker.apiUrl ? new URL(tracker.apiUrl).hostname.replace(/^api\./, "") : "github.com";
    else return undefined;
  } catch {
    return undefined;
  }
  return `git@${host}:${projectKey}.git`;
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
  /**
   * «dispatch»: Mako only dispatches — every task goes to Orca, the tracker's status follows (start, MR,
   * merge); no terminal, plan, autopilot or templates. «full» (default): everything.
   */
  mode: "full" | "dispatch";
  /** Done and cancelled tasks go to the archive this many days after they finished; 0 keeps them on the board. */
  archiveAfterDays: number;
  /** Where repos without a path of their own live (cloned by Mako): ~/mako/projects. */
  projectsDir: string;
  /** The general branch name pattern, for repos Mako sets up itself. */
  branchPattern: string;
  /** Named ways to run an agent; repos pick one with `executor`. */
  executors: Record<string, ExecutorConfig>;
  defaultExecutor: string;
  dryRun: boolean;
  /** Every taken task starts with a plan the developer approves (default); a task can opt out when taken. */
  planFirst: boolean;
  /** Ready-made instructions for recurring kinds of tasks, added to the prompt of the tasks they match. */
  templates: TaskTemplate[];
  /** Mako keeps what agents learn about each repo and gives it to the next task there (default on). */
  memory: boolean;
  /**
   * MR autopilot: failed CI, new review threads and conflicts go to a background agent without a click,
   * at most `maxRounds` times per task; `merge` also merges once the host says the MR is ready.
   */
  autopilot: { enabled: boolean; maxRounds: number; merge: boolean };
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

  const projectsDir = resolvePath(raw.projectsDir || DEFAULT_PROJECTS_DIR);
  const trackers: TrackerConfig[] = (raw.trackers ?? []).map((t: any) => {
    const projects: Record<string, ProjectConfig> = {};
    for (const [key, p] of Object.entries<any>(t.projects ?? {})) {
      // Short form: the project itself is a single repo.
      // An empty `repos` (a repo whose settings are all defaults, from older settings pages) is the short form too.
      const rawRepos: Record<string, any> = p.repos && Object.keys(p.repos).length ? p.repos : { [key.split("/").pop()!]: p };
      const repos: Record<string, RepoConfig> = {};
      const several = rawRepos === p.repos && Object.keys(rawRepos).length > 1;
      for (const [name, r] of Object.entries<any>(rawRepos)) repos[name] = repoConfig(name, r, several ? undefined : defaultCloneUrl(t, key), projectsDir, raw.branchPattern);
      if (p.default && !repos[p.default]) throw new Error(`Project ${key}: default repo "${p.default}" is not in repos`);
      projects[key] = { repos, default: p.default };
    }
    // allProjects: the projects come from the issues assigned to me, so none have to be listed.
    if (!Object.keys(projects).length && !t.allProjects) throw new Error(`Tracker ${t.id ?? t.type} has no projects`);
    return { ...t, projects };
  });
  if (!trackers.length) throw new Error("Config has no trackers");

  // Older configs had a single `claude` block: that is the headless Claude executor.
  const executors: Record<string, ExecutorConfig> = raw.executors ?? {
    claude: { type: "claude-headless", command: raw.claude?.command, args: raw.claude?.args },
  };
  // The dispatcher hands everything to Orca: its executor, made if the config has none.
  const dispatch = raw.mode === "dispatch";
  const orca = dispatch ? (Object.keys(executors).find((n) => (executors as any)[n].type === "orca") ?? "orca") : undefined;
  if (orca && !executors[orca]) (executors as any)[orca] = { type: "orca", agent: "claude" };
  // Without a choice: Mako's own terminal, where the developer sees the agent and can step in.
  const defaultExecutor: string =
    orca ?? raw.defaultExecutor ?? Object.keys(executors).find((n) => (executors as any)[n].type === "terminal") ?? Object.keys(executors)[0];
  if (orca) for (const t of trackers) for (const p of Object.values(t.projects)) for (const r of Object.values(p.repos)) r.executor = orca;
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
    archiveAfterDays: Math.max(0, Number(raw.archiveAfterDays ?? 2) || 0),
    projectsDir,
    branchPattern: (raw.branchPattern || DEFAULT_BRANCH_PATTERN).trim(),
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
    mode: dispatch ? "dispatch" : "full",
    planFirst: dispatch ? false : (raw.planFirst ?? true),
    templates: (raw.templates ?? [])
      .filter((t: any) => t && typeof t.name === "string" && t.name.trim() && typeof t.instructions === "string" && t.instructions.trim())
      .map((t: any) => ({
        name: t.name.trim(),
        instructions: t.instructions.trim(),
        labels: (t.labels ?? []).map((l: string) => String(l).toLowerCase()),
        titleContains: (t.titleContains ?? []).map((w: string) => String(w).toLowerCase()),
      })),
    memory: raw.memory ?? true,
    autopilot: {
      enabled: !dispatch && (raw.autopilot?.enabled ?? false),
      maxRounds: Math.max(1, Number(raw.autopilot?.maxRounds) || 3),
      merge: raw.autopilot?.merge ?? false,
    },
  };
}
