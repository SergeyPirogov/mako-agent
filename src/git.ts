import { existsSync, mkdirSync } from "node:fs";
import path from "node:path";
import type { ProjectConfig } from "./config.js";
import { run, runOk } from "./shell.js";

export function branchFor(issueKey: string): string {
  return `agent/${issueKey}`;
}

/** Creates (or reuses) an isolated worktree for the issue, branched from the latest base branch. */
export async function prepareWorktree(project: ProjectConfig, issueKey: string): Promise<string> {
  const dir = path.join(project.worktreesDir, issueKey);
  const branch = branchFor(issueKey);
  if (existsSync(dir)) return dir;

  mkdirSync(project.worktreesDir, { recursive: true });
  await runOk("git", ["fetch", "origin", project.baseBranch], project.repoPath);

  const branchExists = (await run("git", ["rev-parse", "--verify", "--quiet", branch], project.repoPath)).code === 0;
  const args = branchExists
    ? ["worktree", "add", dir, branch]
    : ["worktree", "add", "-b", branch, dir, `origin/${project.baseBranch}`];
  await runOk("git", args, project.repoPath);
  return dir;
}

export async function commitsAhead(project: ProjectConfig, worktree: string): Promise<number> {
  const out = await runOk("git", ["rev-list", "--count", `origin/${project.baseBranch}..HEAD`], worktree);
  return Number(out);
}

export async function hasUncommittedChanges(worktree: string): Promise<boolean> {
  return (await runOk("git", ["status", "--porcelain"], worktree)).length > 0;
}

export async function commitAll(worktree: string, message: string): Promise<void> {
  await runOk("git", ["add", "-A"], worktree);
  await runOk("git", ["commit", "-m", message], worktree);
}

export async function pushBranch(worktree: string, branch: string): Promise<void> {
  await runOk("git", ["push", "-u", "origin", branch], worktree);
}

/** Opens a PR with the GitHub CLI, or returns the existing one's URL. */
export async function createPullRequest(
  project: ProjectConfig,
  worktree: string,
  branch: string,
  title: string,
  body: string,
): Promise<string> {
  const existing = await run("gh", ["pr", "view", branch, "--json", "url", "-q", ".url"], worktree);
  if (existing.code === 0 && existing.stdout.trim()) return existing.stdout.trim();
  return runOk(
    "gh",
    ["pr", "create", "--base", project.baseBranch, "--head", branch, "--title", title, "--body", body],
    worktree,
  );
}
