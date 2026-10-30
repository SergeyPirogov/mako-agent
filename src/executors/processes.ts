import type { ChildProcess } from "node:child_process";

const killers = new Set<() => void>();

/** Registers a stop function for a running agent so Ctrl+C can stop it. Returns the unregister function. */
export function onShutdown(kill: () => void): () => void {
  killers.add(kill);
  return () => killers.delete(kill);
}

const byTask = new Map<string, () => void>();

/** Registers a managed agent process so Ctrl+C, or the developer for its task, can stop it. */
export function track(child: ChildProcess, taskId?: string): void {
  const kill = () => child.kill("SIGTERM");
  const off = onShutdown(kill);
  if (taskId) byTask.set(taskId, kill);
  child.on("close", () => {
    off();
    if (taskId && byTask.get(taskId) === kill) byTask.delete(taskId);
  });
}

/** Stops the background agent of one task; false when none is running. */
export function stopTask(taskId: string): boolean {
  const kill = byTask.get(taskId);
  kill?.();
  return Boolean(kill);
}

export function killAllAgents(): void {
  for (const kill of killers) kill();
}
