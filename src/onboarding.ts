import { readdirSync, readFileSync, existsSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { findCommand } from "./executors/find-command.js";
import { findOrca } from "./executors/orca.js";
import { run } from "./shell.js";

/** What a new machine needs: git to make worktrees and an agent CLI to run. gh only helps GitHub logins; Orca, if installed, is one more place to hand tasks to. */
export async function doctor(): Promise<{ git?: string; claude?: string; gh?: string; ghLoggedIn: boolean; orca?: string }> {
  const git = findCommand("git");
  const claude = findCommand("claude");
  const gh = findCommand("gh");
  const ghLoggedIn = gh ? (await run(gh, ["auth", "token"], homedir()).catch(() => undefined))?.code === 0 : false;
  return { git, claude, gh, ghLoggedIn, orca: process.platform === "win32" ? undefined : findOrca() };
}

/** Folders where people usually keep their clones, looked through a few levels deep. */
const ROOTS = [
  "", "projects", "Projects", "code", "Code", "src", "dev", "Dev", "Developer", "work", "Work", "repos", "git", "GitHub",
  "IdeaProjects", "WebstormProjects", "PycharmProjects", "Documents", "Documents/GitHub", "Documents/Projects", "Desktop", "go/src",
];
const SKIP = new Set(["node_modules", "Library", "Applications", "Pictures", "Music", "Movies", "Downloads", "vendor", "dist", "build", "target"]);

/**
 * Finds local clones of a tracker project ("group/sub/repo" or "owner/repo") by reading `.git/config`
 * of folders under the usual roots, so a colleague does not have to type the path.
 */
export function findRepos(project: string, limit = 5): string[] {
  const want = project.toLowerCase().replace(/^\/+|\/+$|\.git$/g, "");
  if (!want) return [];
  const home = homedir();
  const found = new Set<string>();
  const seen = new Map<string, number>(); // folder -> depth it was looked through with
  let budget = 6000;
  const walk = (dir: string, depth: number) => {
    if (found.size >= limit || budget-- <= 0 || (seen.get(dir) ?? -1) >= depth) return;
    seen.set(dir, depth);
    if (existsSync(path.join(dir, ".git"))) {
      if (remotePaths(dir).some((p) => p === want || p.endsWith("/" + want))) found.add(dir);
      return; // no clones inside clones
    }
    if (depth === 0) return;
    let entries;
    try {
      entries = readdirSync(dir, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      if (e.isDirectory() && !e.name.startsWith(".") && !SKIP.has(e.name)) walk(path.join(dir, e.name), depth - 1);
    }
  };
  for (const r of ROOTS) walk(path.join(home, r), r ? 3 : 1);
  return [...found];
}

/** Paths of the remotes of a clone: "git@host:group/repo.git" and "https://host/group/repo" give "group/repo". */
function remotePaths(dir: string): string[] {
  let config: string;
  try {
    config = readFileSync(path.join(dir, ".git", "config"), "utf8");
  } catch {
    return []; // a worktree: .git is a file
  }
  return [...config.matchAll(/^\s*url\s*=\s*(.+)$/gm)].map((m) =>
    m[1]
      .trim()
      .replace(/^[\w+.-]+:\/\/[^/]+\//, "") // scheme://[user@]host[:port]/
      .replace(/^[^@/]+@[^:]+:/, "") // user@host:
      .replace(/\.git$/, "")
      .replace(/\/+$/, "")
      .toLowerCase(),
  );
}
