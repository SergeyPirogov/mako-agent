#!/usr/bin/env node
import { parseArgs } from "node:util";
import { loadConfig } from "./config.js";
import { JiraClient } from "./jira.js";
import { StateStore } from "./state.js";
import { Orchestrator } from "./orchestrator.js";
import { killAllAgents } from "./agent.js";
import { log } from "./log.js";

const { values } = parseArgs({
  options: {
    config: { type: "string", short: "c", default: "agent.config.json" },
    once: { type: "boolean", default: false },
    "dry-run": { type: "boolean", default: false },
  },
});

const cfg = loadConfig(values.config!, values["dry-run"]!);
const orchestrator = new Orchestrator(cfg, new JiraClient(cfg.jira), new StateStore(cfg.stateDir));

let shuttingDown = false;
process.on("SIGINT", () => {
  if (shuttingDown) process.exit(1);
  shuttingDown = true;
  log("main", "stopping: terminating running agents (Ctrl+C again to force)");
  orchestrator.stop();
  killAllAgents();
  orchestrator.drain().then(() => process.exit(0));
});

if (values.once) {
  await orchestrator.pollOnce();
  await orchestrator.drain();
} else {
  await orchestrator.start();
}
