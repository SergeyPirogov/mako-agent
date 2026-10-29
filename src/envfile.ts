import { existsSync, readFileSync, writeFileSync, chmodSync } from "node:fs";

/** Secrets the settings page can manage. Values of `*_TOKEN` are never sent back to the browser. */
export const SECRET_NAMES = ["JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN", "GITHUB_TOKEN", "GITLAB_TOKEN"] as const;
export type SecretName = (typeof SECRET_NAMES)[number];

export function isHiddenSecret(name: string): boolean {
  return name.endsWith("_TOKEN");
}

export function loadEnvFile(envPath: string): void {
  if (existsSync(envPath)) process.loadEnvFile(envPath);
}

/**
 * Sets or removes keys in the .env file, keeping every other line (comments, unknown keys) as is,
 * and applies the change to process.env.
 */
export function updateEnvFile(envPath: string, changes: Record<string, string | null>): void {
  const lines = existsSync(envPath) ? readFileSync(envPath, "utf8").split("\n") : [];
  const pending = new Map(Object.entries(changes));
  const out: string[] = [];
  for (const line of lines) {
    const key = line.match(/^\s*([A-Z0-9_]+)\s*=/)?.[1];
    if (key && pending.has(key)) {
      const value = pending.get(key);
      pending.delete(key);
      if (value) out.push(`${key}=${quote(value)}`);
      continue;
    }
    out.push(line);
  }
  while (out.length && out[out.length - 1] === "") out.pop();
  for (const [key, value] of pending) if (value) out.push(`${key}=${quote(value)}`);
  writeFileSync(envPath, out.join("\n") + "\n");
  try {
    chmodSync(envPath, 0o600);
  } catch {
    // Not supported on every filesystem (e.g. Windows); the file still works.
  }
  for (const [key, value] of Object.entries(changes)) {
    if (value) process.env[key] = value;
    else delete process.env[key];
  }
}

function quote(value: string): string {
  return /^[\w@.:/+-]*$/.test(value) ? value : JSON.stringify(value);
}
