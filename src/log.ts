const recent = new Map<string, { at: number; text: string }[]>();
const KEEP = 400;

export function log(scope: string, message: string): void {
  const time = new Date().toISOString().slice(11, 19);
  console.log(`${time} [${scope}] ${message}`);
  let lines = recent.get(scope);
  if (!lines) recent.set(scope, (lines = []));
  lines.push({ at: Date.now(), text: message });
  if (lines.length > KEEP) lines.splice(0, lines.length - KEEP);
}

/** The latest log lines of one scope (a task id), for the board and the task log page. */
export function recentLog(scope: string): { at: number; text: string }[] {
  return recent.get(scope) ?? [];
}
