import { App } from "./app.js";
import { startWebUi } from "./web.js";
import { killAllAgents } from "./executors/index.js";
import { loadEnvFile } from "./envfile.js";
import { openUrl } from "./notify.js";
import { log } from "./log.js";

export interface Runtime {
  app: App;
  /** Port of the web UI, or null when it is disabled. */
  port: number | null;
  url(path?: string): string | undefined;
  shutdown(): Promise<void>;
}

/** Starts Mako: config, orchestrator and web UI. Shared by the CLI and the desktop app. */
export function startMako(opts: { configPath: string; envPath: string; openSettingsOnFirstRun: boolean }): Runtime {
  loadEnvFile(opts.envPath);
  const app = new App(opts.configPath, opts.envPath);
  app.apply();

  const port = app.cfg ? app.cfg.notifications.inboxPort : 7777;
  const url = (p = "/") => (port ? `http://127.0.0.1:${port}${p}` : undefined);
  if (port) {
    startWebUi(app, port);
    if (!app.orchestrator) {
      log("main", `${app.problem}. Open ${url("/settings")}`);
      if (opts.openSettingsOnFirstRun) openUrl(url("/settings")!);
    }
  } else if (!app.orchestrator) {
    throw new Error(app.problem ?? "Mako is not configured");
  }

  return {
    app,
    port,
    url,
    async shutdown() {
      app.orchestrator?.stop();
      killAllAgents();
      await app.orchestrator?.drain();
    },
  };
}
