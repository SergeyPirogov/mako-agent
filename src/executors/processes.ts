import type { ChildProcess } from "node:child_process";

const killers = new Set<() => void>();

/** Registers a stop function for a running agent so Ctrl+C can stop it. Returns the unregister function. */
export function onShutdown(kill: () => void): () => void {
  killers.add(kill);
  return () => killers.delete(kill);
}

/** Registers a managed agent process so Ctrl+C can stop it. */
export function track(child: ChildProcess): void {
  const off = onShutdown(() => child.kill("SIGTERM"));
  child.on("close", off);
}

export function killAllAgents(): void {
  for (const kill of killers) kill();
}
