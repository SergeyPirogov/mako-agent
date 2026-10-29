import type { RepoConfig } from "./config.js";
import { runOk } from "./shell.js";

export interface Review {
  kind: "pull" | "merge-request";
  number: number;
  url?: string;
}

export interface ReviewLookup {
  review?: Review;
  /** Head commit of the task branch, remembered so the PR is still found after the branch is deleted on merge. */
  branchSha?: string;
}

/**
 * Finds a PR/MR opened from the task branch, using only `git ls-remote`:
 * GitHub publishes PR heads as refs/pull/<n>/head, GitLab as refs/merge-requests/<n>/head.
 * Tools may suffix the branch when it already exists (agent/X-1-2), so those count too.
 */
export async function findReview(repo: RepoConfig, branch: string, knownSha?: string): Promise<ReviewLookup> {
  const out = await runOk(
    "git",
    ["ls-remote", "origin", `refs/heads/${branch}*`, "refs/pull/*/head", "refs/merge-requests/*/head"],
    repo.repoPath,
  );
  const branchShas = new Set<string>(knownSha ? [knownSha] : []);
  const reviews: { sha: string; kind: Review["kind"]; number: number }[] = [];
  let branchSha: string | undefined;

  for (const line of out.split("\n")) {
    const [sha, ref] = line.trim().split(/\s+/);
    if (!sha || !ref) continue;
    const head = ref.match(/^refs\/heads\/(.+)$/)?.[1];
    if (head && (head === branch || new RegExp(`^${escapeRegExp(branch)}-\\d+$`).test(head))) {
      branchShas.add(sha);
      if (head === branch || !branchSha) branchSha = sha;
      continue;
    }
    const review = ref.match(/^refs\/(pull|merge-requests)\/(\d+)\/head$/);
    if (review) reviews.push({ sha, kind: review[1] === "pull" ? "pull" : "merge-request", number: Number(review[2]) });
  }

  const match = reviews.filter((r) => branchShas.has(r.sha)).sort((a, b) => b.number - a.number)[0];
  if (!match) return { branchSha };
  const web = webUrl(await runOk("git", ["config", "--get", "remote.origin.url"], repo.repoPath));
  const path = match.kind === "pull" ? "/pull/" : "/-/merge_requests/";
  return { branchSha, review: { kind: match.kind, number: match.number, url: web && `${web}${path}${match.number}` } };
}

/** git@host:group/repo.git, ssh://git@host:22/group/repo.git, https://host/group/repo.git → https://host/group/repo */
export function webUrl(remote: string): string | undefined {
  const r = remote.trim().replace(/\.git$/, "");
  const scp = r.match(/^[\w.-]+@([^:/]+):(.+)$/);
  if (scp) return `https://${scp[1]}/${scp[2]}`;
  const url = r.match(/^(?:ssh|git|https?):\/\/(?:[^@/]+@)?([^:/]+)(?::\d+)?\/(.+)$/);
  if (url) return `https://${url[1]}/${url[2]}`;
  return undefined;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
