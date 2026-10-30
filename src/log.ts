import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync } from "node:fs";
import path from "node:path";

const recent = new Map<string, { at: number; text: string }[]>();
const KEEP = 400;
/** Where task activity is kept between runs; set once the state folder is known. */
let dir: string | undefined;

/** Keeps every task's activity on disk too, so «Хід роботи» survives a restart of Mako. */
export function keepLogsIn(folder: string): void {
  dir = folder;
  mkdirSync(dir, { recursive: true });
}

const fileFor = (scope: string) => (dir ? path.join(dir, `${scope.replace(/[^\w.-]+/g, "_")}.log`) : undefined);
// Task ids look like "group/project#12" or "PROJ-12"; Mako's own scopes ("main", "ui", tracker ids) stay in memory.
const isTask = (scope: string) => /#\d+$|^[A-Z][A-Z0-9_]+-\d+$/.test(scope);

export function log(scope: string, message: string): void {
  const time = new Date().toISOString().slice(11, 19);
  console.log(`${time} [${scope}] ${message}`);
  const lines = load(scope);
  const entry = { at: Date.now(), text: message };
  lines.push(entry);
  if (lines.length > KEEP) lines.splice(0, lines.length - KEEP);
  const file = isTask(scope) ? fileFor(scope) : undefined;
  if (file) {
    try {
      appendFileSync(file, JSON.stringify(entry) + "\n");
    } catch {
      // the board keeps working from memory
    }
  }
}

/** The latest log lines of one scope (a task id), for the board and the task log page. */
export function recentLog(scope: string): { at: number; text: string }[] {
  return load(scope);
}

/** Forgets a task's activity, on disk too (the task was deleted). */
export function dropLog(scope: string): void {
  recent.delete(scope);
  const file = fileFor(scope);
  if (file) rmSync(file, { force: true });
}

function load(scope: string): { at: number; text: string }[] {
  let lines = recent.get(scope);
  if (lines) return lines;
  lines = [];
  const file = isTask(scope) ? fileFor(scope) : undefined;
  if (file && existsSync(file)) {
    for (const line of readFileSync(file, "utf8").split("\n").slice(-KEEP - 1)) {
      try {
        if (line) lines.push(JSON.parse(line));
      } catch {
        // a line cut by a crash
      }
    }
  }
  recent.set(scope, lines);
  return lines;
}
