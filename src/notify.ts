import { spawn } from "node:child_process";
import notifier from "node-notifier";
import type { NotificationConfig } from "./config.js";
import type { Task, Tracker } from "./types.js";
import { log } from "./log.js";

export type Decision = "take" | "later" | "ignored";

const TAKE = "Взяти";
const LATER = "Відкласти";
const APP = "Mako";

/** Shows "У вас нова задача" with Take / Postpone buttons and resolves with the user's choice. */
export function askToTake(task: Task, tracker: Tracker, route: string | undefined, cfg: NotificationConfig): Promise<Decision> {
  const title = "🦈 У вас нова задача";
  const inbox = cfg.inboxPort ? `\nВхідні: http://127.0.0.1:${cfg.inboxPort}` : "";
  const message = `${task.id}: ${task.title}\n${tracker.name}${route ? ` → ${route}` : ""}${inbox}`;
  return process.platform === "linux"
    ? askLinux(title, message, cfg.timeoutSec)
    : askNodeNotifier(title, message, cfg.timeoutSec);
}

/** Fire-and-forget notification without buttons. */
export function inform(title: string, message: string): void {
  if (process.platform === "linux") {
    spawn("notify-send", ["--app-name", APP, title, message], { stdio: "ignore" }).on("error", () => {});
  } else {
    notifier.notify({ title, message, sound: true });
  }
}

/** libnotify >= 0.7.9: `--action` buttons, and `--wait` prints the clicked action's key. */
function askLinux(title: string, message: string, timeoutSec: number): Promise<Decision> {
  return new Promise((resolve) => {
    const child = spawn(
      "notify-send",
      [
        "--app-name", APP,
        "--urgency", "critical",
        "--wait",
        "--action", `take=${TAKE}`,
        "--action", `later=${LATER}`,
        title,
        message,
      ],
      { stdio: ["ignore", "pipe", "ignore"] },
    );
    let out = "";
    child.stdout.on("data", (d) => (out += d));
    const timer = setTimeout(() => child.kill(), timeoutSec * 1000);
    child.on("error", (err) => {
      clearTimeout(timer);
      log("notify", `notify-send unavailable (${err.message}); use the inbox page`);
      resolve("ignored");
    });
    child.on("close", () => {
      clearTimeout(timer);
      const action = out.trim();
      resolve(action === "take" ? "take" : action === "later" ? "later" : "ignored");
    });
  });
}

/** macOS (terminal-notifier) and Windows (SnoreToast) both support action buttons through node-notifier. */
function askNodeNotifier(title: string, message: string, timeoutSec: number): Promise<Decision> {
  return new Promise((resolve) => {
    notifier.notify(
      { title, message, actions: [TAKE, LATER], wait: true, timeout: timeoutSec, sound: true } as any,
      (err: Error | null, response: string, metadata?: any) => {
        if (err) {
          log("notify", `notification failed (${err.message}); use the inbox page`);
          return resolve("ignored");
        }
        const answer = [response, metadata?.activationValue, metadata?.button]
          .filter(Boolean)
          .map((s: string) => s.toLowerCase());
        if (answer.includes(TAKE.toLowerCase())) resolve("take");
        else if (answer.includes(LATER.toLowerCase())) resolve("later");
        else resolve("ignored");
      },
    );
  });
}
