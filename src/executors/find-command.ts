import { accessSync, constants, statSync } from "node:fs";
import os from "node:os";
import path from "node:path";

/**
 * Finds a CLI as an absolute path: as given when it is a path, otherwise in PATH, the usual install folders
 * and `extra` candidates. Apps started from Finder see a minimal PATH, so the folders matter there.
 */
export function findCommand(command: string, extra: string[] = []): string | undefined {
  const home = os.homedir();
  const expanded = command.startsWith("~/") ? path.join(home, command.slice(2)) : command;
  if (expanded.includes("/")) return isExecutable(expanded) ? expanded : undefined;
  const dirs = [
    ...(process.env.PATH ?? "").split(path.delimiter),
    path.join(home, ".local/bin"),
    path.join(home, ".claude/local"),
    path.join(home, ".npm-global/bin"),
    path.join(home, ".bun/bin"),
    path.join(home, ".volta/bin"),
    "/opt/homebrew/bin",
    "/usr/local/bin",
  ].filter(Boolean);
  for (const dir of dirs) {
    const candidate = path.join(dir, expanded);
    if (isExecutable(candidate)) return candidate;
  }
  return extra.find(isExecutable);
}

function isExecutable(file: string): boolean {
  try {
    accessSync(file, constants.X_OK);
    return statSync(file).isFile();
  } catch {
    return false;
  }
}
