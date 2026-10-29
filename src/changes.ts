import { existsSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { run } from "./shell.js";

export interface ChangedFile {
  path: string;
  /** A added, M modified, D deleted, R renamed, ? new and not yet committed */
  status: string;
  additions: number;
  deletions: number;
}

export interface Changes {
  base: string;
  commits: { hash: string; subject: string }[];
  files: ChangedFile[];
  /** Unified diff of everything the task changed, committed or not; capped for huge changes. */
  diff: string;
  truncated: boolean;
}

const MAX_DIFF = 1_500_000;
const MAX_NEW_FILE = 200_000;

/** What the task changed compared with its base branch: commits, files and the diff (including uncommitted work). */
export async function collectChanges(worktree: string, baseBranch: string): Promise<Changes> {
  if (!existsSync(worktree)) throw new Error(`Worktree не знайдено: ${worktree}`);
  const git = async (...args: string[]) => {
    const r = await run("git", args, worktree);
    if (r.code !== 0) throw new Error(`git ${args[0]}: ${r.stderr.trim()}`);
    return r.stdout;
  };
  const base =
    (await run("git", ["merge-base", "HEAD", `origin/${baseBranch}`], worktree)).stdout.trim() ||
    (await run("git", ["rev-parse", `origin/${baseBranch}`], worktree)).stdout.trim() ||
    "HEAD";

  const commits = (await git("log", "--format=%h%x09%s", `${base}..HEAD`))
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      const [hash, ...rest] = line.split("\t");
      return { hash, subject: rest.join("\t") };
    });

  const stats = new Map<string, { additions: number; deletions: number }>();
  for (const line of (await git("diff", "--numstat", "-M", base)).split("\n").filter(Boolean)) {
    const [add, del, ...p] = line.split("\t");
    stats.set(renamedTo(p.join("\t")), { additions: Number(add) || 0, deletions: Number(del) || 0 });
  }
  const files: ChangedFile[] = (await git("diff", "--name-status", "-M", base))
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      const [status, ...paths] = line.split("\t");
      const file = paths[paths.length - 1];
      return { path: file, status: status[0], ...(stats.get(file) ?? { additions: 0, deletions: 0 }) };
    });

  let diff = await git("diff", "-M", "--no-color", base);

  // New files the agent has not committed yet are invisible to git diff; show them as added.
  const untracked = (await git("ls-files", "--others", "--exclude-standard")).split("\n").filter(Boolean);
  for (const file of untracked) {
    const full = path.join(worktree, file);
    let body = "";
    let lines = 0;
    try {
      const size = statSync(full).size;
      const content = size <= MAX_NEW_FILE ? readFileSync(full) : undefined;
      if (!content) body = `Binary files /dev/null and b/${file} differ\n`;
      else if (content.includes(0)) body = `Binary files /dev/null and b/${file} differ\n`;
      else {
        const text = content.toString("utf8").replace(/\n$/, "");
        const all = text ? text.split("\n") : [];
        lines = all.length;
        body = `--- /dev/null\n+++ b/${file}\n@@ -0,0 +1,${lines} @@\n${all.map((l) => "+" + l).join("\n")}\n`;
      }
    } catch {
      continue;
    }
    files.push({ path: file, status: "?", additions: lines, deletions: 0 });
    diff += `diff --git a/${file} b/${file}\nnew file (not committed yet)\n${body}`;
  }

  const truncated = diff.length > MAX_DIFF;
  return { base: baseBranch, commits, files, diff: truncated ? diff.slice(0, MAX_DIFF) : diff, truncated };
}

/** numstat shows renames as "dir/{old => new}.ts" or "old => new"; keep the new path. */
function renamedTo(p: string): string {
  const braces = p.match(/^(.*)\{(.*) => (.*)\}(.*)$/);
  if (braces) return (braces[1] + braces[3] + braces[4]).replace(/\/\//g, "/");
  const plain = p.split(" => ");
  return plain[plain.length - 1];
}
