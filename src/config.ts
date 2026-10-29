import { readFileSync, existsSync } from "node:fs";
import path from "node:path";
import type { TrackerConfig } from "./trackers/index.js";

export interface ProjectConfig {
  repoPath: string;
  baseBranch: string;
  worktreesDir: string;
  /** Where to open the pull/merge request; "auto" picks by the origin remote URL. */
  pullRequest: "auto" | "github" | "gitlab" | "none";
  extraInstructions?: string;
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
      projects[key] = {
        repoPath: path.resolve(p.repoPath),
        baseBranch: p.baseBranch ?? "main",
        worktreesDir: path.resolve(p.worktreesDir ?? path.join(p.repoPath, "..", ".worktrees")),
        pullRequest: p.pullRequest ?? "auto",
        extraInstructions: p.extraInstructions,
      };
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
