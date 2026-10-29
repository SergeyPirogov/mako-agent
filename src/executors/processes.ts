import type { ChildProcess } from "node:child_process";

const running = new Set<ChildProcess>();

/** Registers a managed agent process so Ctrl+C can stop it. */
export function track(child: ChildProcess): void {
  running.add(child);
  child.on("close", () => running.delete(child));
}

export function killAllAgents(): void {
  for (const child of running) child.kill("SIGTERM");
}
