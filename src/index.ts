#!/usr/bin/env node
import { parseArgs } from "node:util";
import { loadConfig, env } from "./config.js";
import { createTracker } from "./trackers/index.js";
import { StateStore } from "./state.js";
import { Orchestrator } from "./orchestrator.js";
import { startInbox } from "./inbox.js";
import { killAllAgents } from "./executors/index.js";
import { log } from "./log.js";

const { values } = parseArgs({
  options: {
    config: { type: "string", short: "c", default: "mako.config.json" },
    "dry-run": { type: "boolean", default: false },
  },
});

const cfg = loadConfig(values.config!, values["dry-run"]!);
const trackers = cfg.trackers.map((t) => createTracker(t, env));
const orchestrator = new Orchestrator(cfg, trackers, new StateStore(cfg.stateDir));

if (cfg.dryRun) {
  await orchestrator.poll();
  process.exit(0);
}

let shuttingDown = false;
process.on("SIGINT", () => {
  if (shuttingDown) process.exit(1);
  shuttingDown = true;
  log("main", "stopping: terminating running agents (Ctrl+C again to force)");
  orchestrator.stop();
  killAllAgents();
  orchestrator.drain().then(() => process.exit(0));
});

if (cfg.notifications.inboxPort) startInbox(orchestrator, cfg.notifications.inboxPort);
await orchestrator.start();
