import { existsSync, mkdirSync } from "node:fs";
import path from "node:path";
import type { RepoConfig } from "./config.js";
import { run, runOk } from "./shell.js";

export function branchFor(slug: string): string {
  return `agent/${slug}`;
}

/** Creates (or reuses) an isolated worktree for the task, branched from the latest base branch. */
export async function prepareWorktree(repo: RepoConfig, slug: string): Promise<string> {
  const dir = path.join(repo.worktreesDir, slug);
  const branch = branchFor(slug);
  if (existsSync(dir)) return dir;

  mkdirSync(repo.worktreesDir, { recursive: true });
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
): Promise<string | undefined> {
  const host = repo.pullRequest === "auto" ? await detectHost(worktree) : repo.pullRequest;

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
