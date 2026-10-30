import { existsSync } from "node:fs";
import path from "node:path";
import { parseConfig, readRawConfig, writeRawConfig, type Config } from "./config.js";
import { SECRET_NAMES, isHiddenSecret, updateEnvFile } from "./envfile.js";
import { createTracker, type TrackerConfig } from "./trackers/index.js";
import { createExecutor } from "./executors/index.js";
import { StateStore } from "./state.js";
import { Orchestrator } from "./orchestrator.js";
import type { Tracker } from "./types.js";
import { log } from "./log.js";

export type SecretChanges = Record<string, string | null>;

/** Owns the config lifecycle: first-run setup, validation, saving and live reload. */
export class App {
  orchestrator?: Orchestrator;
  cfg?: Config;
  /** Why Mako is not running yet (no config, invalid config). */
  problem?: string;

  constructor(
    readonly configPath: string,
    readonly envPath: string,
  ) {}

  /** Loads the config from disk and starts or reloads the orchestrator. */
  apply(): void {
    let raw: any;
    try {
      raw = readRawConfig(this.configPath);
    } catch (err: any) {
      this.problem = `${this.configPath}: ${err.message}`;
      return;
    }
    if (!raw) {
      this.problem = "Mako ще не налаштований";
      return;
    }
    try {
      const { cfg, trackers } = build(raw, envFrom({}));
      this.cfg = cfg;
      this.problem = undefined;
      if (this.orchestrator) {
        this.orchestrator.reload(cfg, trackers);
        log("main", "config reloaded");
      } else {
        this.orchestrator = new Orchestrator(cfg, trackers, new StateStore(cfg.stateDir));
        void this.orchestrator.start();
      }
    } catch (err: any) {
      this.problem = err.message;
      log("main", `config error: ${err.message}`);
    }
  }

  /** What the settings page edits: the raw config (or a starter one) and which secrets are set. */
  settings() {
    const raw = readRawConfig(this.configPath);
    const secrets = Object.fromEntries(
      SECRET_NAMES.map((name) => {
        const value = process.env[name];
        return [name, { set: Boolean(value), value: value && !isHiddenSecret(name) ? value : undefined }];
      }),
    );
    return {
      configured: Boolean(raw),
      problem: this.problem,
      configPath: path.resolve(this.configPath),
      config: raw ?? starterConfig(),
      secrets,
    };
  }

  /** Validates first (with the new secrets), then writes .env and the config and applies them. */
  save(raw: any, secrets: SecretChanges): { warnings: string[] } {
    const { cfg } = build(raw, envFrom(secrets));
    const changes = Object.fromEntries(
      Object.entries(secrets).filter(([name]) => (SECRET_NAMES as readonly string[]).includes(name)),
    );
    if (Object.keys(changes).length) updateEnvFile(this.envPath, changes);
    writeRawConfig(this.configPath, raw);
    this.apply();
    return { warnings: repoWarnings(cfg) };
  }

  /** Projects one tracker's token can see, for the project field of the settings page. */
  async trackerProjects(rawTracker: any, secrets: SecretChanges): Promise<{ key: string; name: string }[]> {
    // The project list does not depend on the projects already set, which may still be empty.
    const { trackers } = build(
      { trackers: [{ ...rawTracker, projects: { "-": { repoPath: "~" } } }], executors: { test: { type: "command", command: "true" } } },
      envFrom(secrets),
    );
    return (await trackers[0].listProjects?.()) ?? [];
  }

  /** Fetches tasks with one tracker's settings, so the page can show "found N tasks". */
  async testTracker(
    rawTracker: any,
    secrets: SecretChanges,
  ): Promise<{ count: number; sample: { id: string; title: string }[]; statuses?: string[]; statusError?: string; filter?: string }> {
    const { trackers } = build(
      { trackers: [rawTracker], executors: { test: { type: "command", command: "true" } } },
      envFrom(secrets),
    );
    const tasks = await trackers[0].fetchTasks();
    let statuses: string[] | undefined;
    let statusError: string | undefined;
    if (trackers[0].listStatuses) {
      try {
        statuses = await trackers[0].listStatuses();
      } catch (err: any) {
        statusError = err.message;
      }
    }
    const filter = await trackers[0].describeFilter?.().catch(() => undefined);
    return { count: tasks.length, sample: tasks.slice(0, 5).map((t) => ({ id: t.id, title: t.title })), statuses, statusError, filter };
  }
}

/** Parses the config and constructs trackers and executors, which surfaces missing secrets and bad types. */
function build(raw: any, env: (name: string) => string): { cfg: Config; trackers: Tracker[] } {
  const cfg = parseConfig(raw);
  const ids = new Set<string>();
  const trackers = cfg.trackers.map((t: TrackerConfig) => {
    const tracker = createTracker(t, env);
    if (ids.has(tracker.id)) throw new Error(`Two trackers share the id "${tracker.id}"; give one of them a different "id"`);
    ids.add(tracker.id);
    return tracker;
  });
  for (const [name, c] of Object.entries(cfg.executors)) createExecutor(name, c);
  return { cfg, trackers };
}

/** Reads env vars with unsaved changes from the settings page applied on top. */
function envFrom(changes: SecretChanges): (name: string) => string {
  return (name) => {
    const value = name in changes ? changes[name] : process.env[name];
    if (!value) throw new Error(`Missing ${name}: set it in the settings page (Секрети) or in .env`);
    return value;
  };
}

function repoWarnings(cfg: Config): string[] {
  const warnings: string[] = [];
  for (const t of cfg.trackers)
    for (const [key, p] of Object.entries(t.projects))
      for (const r of Object.values(p.repos))
        if (!existsSync(path.join(r.repoPath, ".git")) && !r.cloneUrl)
          warnings.push(`${key} / ${r.name}: ${r.repoPath} не схожий на git-репозиторій`);
  return warnings;
}

function starterConfig() {
  return {
    pollIntervalSec: 60,
    wipLimit: 2,
    notifications: { enabled: true, autoTake: false, snoozeMin: 60 },
    executors: {
      terminal: { type: "terminal" },
      claude: {
        type: "claude-headless",
        args: [
          "--permission-mode", "acceptEdits",
          "--allowedTools", "Bash(git:*) Bash(npm:*) Bash(npx:*) Bash(node:*) Bash(pnpm:*) Bash(yarn:*) Bash(bun:*) Read Edit Write Glob Grep",
          "--max-turns", "80",
        ],
      },
    },
    defaultExecutor: "terminal",
    trackers: [],
  };
}
