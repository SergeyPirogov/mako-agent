#!/usr/bin/env node
import { spawn } from "node:child_process";
import { parseArgs } from "node:util";
import { loadConfig, env } from "./config.js";
import { loadEnvFile } from "./envfile.js";
import { createTracker } from "./trackers/index.js";
import { StateStore } from "./state.js";
import { Orchestrator } from "./orchestrator.js";
import { App } from "./app.js";
import { startWebUi } from "./web.js";
import { killAllAgents } from "./executors/index.js";
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

const app = new App(values.config!, values.env!);
app.apply();

const port = app.cfg ? app.cfg.notifications.inboxPort : 7777;
if (port) {
  startWebUi(app, port);
  if (!app.orchestrator) {
    log("main", `${app.problem}. Open http://127.0.0.1:${port}/settings`);
    if (!values["no-open"]) openBrowser(`http://127.0.0.1:${port}/settings`);
  }
} else if (!app.orchestrator) {
  log("main", `${app.problem}`);
  process.exit(1);
}

let shuttingDown = false;
process.on("SIGINT", () => {
  if (shuttingDown) process.exit(1);
  shuttingDown = true;
  log("main", "stopping: terminating running agents (Ctrl+C again to force)");
  app.orchestrator?.stop();
  killAllAgents();
  (app.orchestrator?.drain() ?? Promise.resolve()).then(() => process.exit(0));
});

function openBrowser(url: string): void {
  const [cmd, args] =
    process.platform === "darwin" ? ["open", [url]] : process.platform === "win32" ? ["cmd", ["/c", "start", "", url]] : ["xdg-open", [url]];
  spawn(cmd as string, args as string[], { stdio: "ignore", detached: true }).on("error", () => {}).unref();
}
