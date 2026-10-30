import { existsSync, mkdirSync, rmSync } from "node:fs";
import { spawn } from "node:child_process";
import path from "node:path";
import type { RepoConfig } from "./config.js";
import type { Task } from "./types.js";
import { run, runOk } from "./shell.js";

/** The branch name of tasks started before branch patterns; still used to find their branches. */
export function branchFor(slug: string): string {
  return `agent/${slug}`;
}

const TRANSLIT: Record<string, string> = {
  а: "a", б: "b", в: "v", г: "h", ґ: "g", д: "d", е: "e", є: "ye", ж: "zh", з: "z", и: "y", і: "i", ї: "yi", й: "i",
  к: "k", л: "l", м: "m", н: "n", о: "o", п: "p", р: "r", с: "s", т: "t", у: "u", ф: "f", х: "kh", ц: "ts", ч: "ch",
  ш: "sh", щ: "shch", ь: "", ю: "yu", я: "ya", ы: "y", э: "e", ё: "yo", ъ: "",
};

/** «Розібрати нічний ран» → "rozibraty-nichnyi-ran": a short, git-safe form of a title. */
export function slugifyTitle(title: string, max = 40): string {
  const latin = title.toLowerCase().replace(/[а-яіїєґёъыэ]/g, (c) => TRANSLIT[c] ?? "");
  const words = latin.replace(/[^a-z0-9]+/g, " ").trim().split(/\s+/).filter(Boolean);
  let out = "";
  for (const w of words) {
    if (out && out.length + 1 + w.length > max) break;
    out = out ? `${out}-${w}` : w.slice(0, max);
  }
  return out || "task";
}

/** fix for bugs (labels, Jira issue type, a title starting with "fix"), feat otherwise. */
export function taskType(task: Task): "fix" | "feat" {
  const text = [...task.labels, task.meta ?? ""].join(" ").toLowerCase();
  if (/\b(bug|fix|defect|hotfix|баг|помилка)\b/.test(text) || /^\s*(fix|bug)/i.test(task.title)) return "fix";
  return "feat";
}

/**
 * The branch for a task from a pattern like "feature/{id}-{title}"; a fresh attempt gets "-try2".
 * {id} is the issue number (GitLab/GitHub) or key (Jira).
 */
export function branchName(pattern: string, task: Task, attempt?: number): string {
  const id = task.id.includes("#") ? task.id.split("#").pop()! : task.id;
  const name = pattern
    .replace(/\{type\}/g, taskType(task))
    .replace(/\{id\}/g, id)
    .replace(/\{title\}/g, slugifyTitle(task.title))
    .replace(/\{slug\}/g, task.slug.replace(/-try\d+$/, ""))
    // git refuses these in names
    .replace(/[\s~^:?*\[\\]+/g, "-")
    .replace(/\.{2,}|@\{/g, "-")
    .replace(/\/{2,}/g, "/")
    .replace(/^[/.-]+|[/.-]+$/g, "");
  return attempt && attempt > 1 ? `${name}-try${attempt}` : name;
}

/**
 * Removes a task's worktree and its local branch (the pushed branch and any PR/MR stay). Never touches the repo
 * itself: executors without worktrees record the repo path as the task's folder.
 */
export async function removeWorktree(repo: RepoConfig, dir: string, branch: string): Promise<boolean> {
  const target = path.resolve(dir);
  const main = path.resolve(repo.repoPath);
  if (target === main || main.startsWith(target + path.sep)) return false;
  let removed = false;
  if (existsSync(target)) {
    const r = await run("git", ["worktree", "remove", "--force", target], main);
    removed = r.code === 0;
    // Not a registered worktree (left over from a crash): delete it only inside Mako's own worktrees folder.
    if (!removed && target.startsWith(path.resolve(repo.worktreesDir) + path.sep)) {
      rmSync(target, { recursive: true, force: true });
      removed = true;
    }
  }
  await run("git", ["worktree", "prune"], main);
  await run("git", ["branch", "-D", branch], main);
  return removed;
}

/** Creates (or reuses) an isolated worktree for the task, branched from the latest base branch. */
/** Creates (or reuses) the task's worktree and, when new, runs the repo's setup command in it. */
export async function prepareWorktree(repo: RepoConfig, slug: string, log?: (line: string) => void, branch?: string): Promise<string> {
  const fresh = !existsSync(path.join(repo.worktreesDir, slug));
  const dir = await createWorktree(repo, slug, branch ?? branchFor(slug));
  if (fresh) {
    const command = setupCommand(repo, dir);
    if (command) {
      log?.(`preparing worktree: ${command}`);
      const r = await runShell(command, dir, 15 * 60_000);
      log?.(r.ok ? "worktree ready" : `setup failed (the agent continues without it): ${r.output.slice(-500)}`);
    }
  }
  return dir;
}

/** The setup command for a worktree: the configured one, or one guessed from the lock file for "auto". */
export function setupCommand(repo: RepoConfig, dir: string): string | undefined {
  if (repo.setup === "none") return undefined;
  if (repo.setup !== "auto") return repo.setup;
  const has = (f: string) => existsSync(path.join(dir, f));
  if (has("pnpm-lock.yaml")) return "pnpm install --frozen-lockfile";
  if (has("bun.lock") || has("bun.lockb")) return "bun install --frozen-lockfile";
  if (has("yarn.lock")) return "yarn install";
  if (has("package-lock.json")) return "npm ci";
  if (has("uv.lock")) return "uv sync";
  if (has("poetry.lock")) return "poetry install";
  if (has("Gemfile.lock")) return "bundle install";
  return undefined;
}

function runShell(command: string, cwd: string, timeoutMs: number): Promise<{ ok: boolean; output: string }> {
  return new Promise((resolve) => {
    const child = spawn(command, { cwd, shell: true, stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, CI: "1" } });
    let output = "";
    const add = (d: Buffer) => (output = (output + d).slice(-20_000));
    child.stdout.on("data", add);
    child.stderr.on("data", add);
    const timer = setTimeout(() => child.kill("SIGTERM"), timeoutMs);
    child.on("error", (err) => {
      clearTimeout(timer);
      resolve({ ok: false, output: err.message });
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      resolve({ ok: code === 0, output });
    });
  });
}

async function createWorktree(repo: RepoConfig, slug: string, branch: string): Promise<string> {
  const dir = path.join(repo.worktreesDir, slug);
  if (existsSync(dir)) return dir;

  mkdirSync(repo.worktreesDir, { recursive: true });
  // Forget worktrees whose folders were deleted by hand, so the task's path can be reused.
  await runOk("git", ["worktree", "prune"], repo.repoPath);
  await runOk("git", ["fetch", "origin", repo.baseBranch], repo.repoPath);

  const branchExists = (await run("git", ["rev-parse", "--verify", "--quiet", branch], repo.repoPath)).code === 0;
  const args = branchExists
    ? ["worktree", "add", dir, branch]
    : ["worktree", "add", "-b", branch, dir, `origin/${repo.baseBranch}`];
  await runOk("git", args, repo.repoPath);
  return dir;
}

export async function commitsAhead(repo: RepoConfig, worktree: string): Promise<number> {
  const out = await runOk("git", ["rev-list", "--count", `origin/${repo.baseBranch}..HEAD`], worktree);
  return Number(out);
}

export async function hasUncommittedChanges(worktree: string): Promise<boolean> {
  return (await runOk("git", ["status", "--porcelain"], worktree)).length > 0;
}

/** The branch checked out in a worktree, if it exists. */
export async function currentBranch(dir: string): Promise<string | undefined> {
  if (!existsSync(dir)) return undefined;
  const r = await run("git", ["rev-parse", "--abbrev-ref", "HEAD"], dir).catch(() => undefined);
  const name = r?.code === 0 ? r.stdout.trim() : "";
  return name && name !== "HEAD" ? name : undefined;
}

export async function stageAll(worktree: string): Promise<void> {
  await runOk("git", ["add", "-A"], worktree);
}

export async function headSha(worktree: string): Promise<string> {
  return runOk("git", ["rev-parse", "HEAD"], worktree);
}

export async function commitAll(worktree: string, message: string): Promise<void> {
  await runOk("git", ["add", "-A"], worktree);
  await runOk("git", ["commit", "-m", message], worktree);
}

export interface PublishRequest {
  title: string;
  body: string;
  /** Single line for hosts that only take a short description (GitLab push options). */
  shortBody: string;
}

/** Pushes the branch and opens a PR/MR when the project asks for one. Returns its URL, if any. */
export async function publish(
  repo: RepoConfig,
  worktree: string,
  branch: string,
  req: PublishRequest,
  /** The kind of tracker the task came from: a GitLab tracker means a GitLab remote, whatever its domain. */
  hostHint?: "github" | "gitlab",
): Promise<string | undefined> {
  const host = repo.pullRequest === "auto" ? hostHint ?? (await detectHost(worktree)) : repo.pullRequest;

  if (host === "gitlab") {
    // GitLab creates the MR itself from push options, so no extra CLI is needed.
    const r = await run(
      "git",
      [
        "push", "-u", "origin", branch,
        "-o", "merge_request.create",
        "-o", `merge_request.target=${repo.baseBranch}`,
        "-o", `merge_request.title=${req.title}`,
        "-o", `merge_request.description=${req.shortBody}`,
      ],
      worktree,
    );
    if (r.code !== 0) throw new Error(`git push failed: ${r.stderr}`);
    return (r.stderr + r.stdout).match(/https?:\/\/\S+\/-\/merge_requests\/\d+/)?.[0];
  }

  await runOk("git", ["push", "-u", "origin", branch], worktree);
  if (host !== "github") return undefined;

  const existing = await run("gh", ["pr", "view", branch, "--json", "url", "-q", ".url"], worktree);
  if (existing.code === 0 && existing.stdout.trim()) return existing.stdout.trim();
  return runOk(
    "gh",
    ["pr", "create", "--base", repo.baseBranch, "--head", branch, "--title", req.title, "--body", req.body],
    worktree,
  );
}

async function detectHost(worktree: string): Promise<"github" | "gitlab" | "none"> {
  const url = await runOk("git", ["remote", "get-url", "origin"], worktree);
  if (url.includes("github")) return "github";
  if (url.includes("gitlab")) return "gitlab";
  return "none";
}
