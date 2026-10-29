import { readFileSync, existsSync } from "node:fs";
import path from "node:path";

export interface ProjectConfig {
  repoPath: string;
  baseBranch: string;
  worktreesDir: string;
  createPullRequest: boolean;
  extraInstructions?: string;
}

export interface Config {
  jira: { baseUrl: string; email: string; apiToken: string };
  pollIntervalSec: number;
  maxConcurrent: number;
  taskTimeoutMin: number;
  stateDir: string;
  jql: string;
  transitions: { onStart: string | null; onSuccess: string | null; onFailure: string | null };
  projects: Record<string, ProjectConfig>;
  claude: { command: string; args: string[] };
  dryRun: boolean;
}

function requireEnv(name: string): string {
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

  const projects: Record<string, ProjectConfig> = {};
  for (const [key, p] of Object.entries<any>(raw.projects ?? {})) {
    projects[key] = {
      repoPath: path.resolve(p.repoPath),
      baseBranch: p.baseBranch ?? "main",
      worktreesDir: path.resolve(p.worktreesDir ?? path.join(p.repoPath, "..", ".worktrees")),
      createPullRequest: p.createPullRequest ?? true,
      extraInstructions: p.extraInstructions,
    };
  }
  if (Object.keys(projects).length === 0) throw new Error("Config has no projects");

  return {
    jira: {
      baseUrl: requireEnv("JIRA_BASE_URL").replace(/\/$/, ""),
      email: requireEnv("JIRA_EMAIL"),
      apiToken: requireEnv("JIRA_API_TOKEN"),
    },
    pollIntervalSec: raw.pollIntervalSec ?? 60,
    maxConcurrent: raw.maxConcurrent ?? 1,
    taskTimeoutMin: raw.taskTimeoutMin ?? 45,
    stateDir: path.resolve(raw.stateDir ?? ".agent-state"),
    jql: raw.jql,
    transitions: {
      onStart: raw.transitions?.onStart ?? null,
      onSuccess: raw.transitions?.onSuccess ?? null,
      onFailure: raw.transitions?.onFailure ?? null,
    },
    projects,
    claude: { command: raw.claude?.command ?? "claude", args: raw.claude?.args ?? [] },
    dryRun,
  };
}
