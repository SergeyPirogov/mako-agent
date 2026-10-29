#!/usr/bin/env node
import { parseArgs } from "node:util";
import { loadConfig, env } from "./config.js";
import { loadEnvFile } from "./envfile.js";
import { createTracker } from "./trackers/index.js";
import { StateStore } from "./state.js";
import { Orchestrator } from "./orchestrator.js";
import { startMako } from "./runtime.js";
import { log } from "./log.js";

const { values } = parseArgs({
  options: {
    config: { type: "string", short: "c", default: "mako.config.json" },
    env: { type: "string", default: ".env" },
    "dry-run": { type: "boolean", default: false },
    "no-open": { type: "boolean", default: false },
  },
});
loadEnvFile(values.env!);

if (values["dry-run"]) {
  const cfg = loadConfig(values.config!, true);
  const orchestrator = new Orchestrator(cfg, cfg.trackers.map((t) => createTracker(t, env)), new StateStore(cfg.stateDir));
  await orchestrator.poll();
  process.exit(0);
}

let runtime;
try {
  runtime = startMako({ configPath: values.config!, envPath: values.env!, openSettingsOnFirstRun: !values["no-open"] });
} catch (err: any) {
  log("main", err.message);
  process.exit(1);
}

let shuttingDown = false;
process.on("SIGINT", () => {
  if (shuttingDown) process.exit(1);
  shuttingDown = true;
  log("main", "stopping: terminating running agents (Ctrl+C again to force)");
  runtime.shutdown().then(() => process.exit(0));
});
