import type { ProjectConfig, RepoConfig } from "./config.js";
import type { Task } from "./types.js";

export const REPO_LABEL_PREFIX = "repo:";

export type Route = { repo: RepoConfig; reason: string } | { repo?: undefined; reason: string };

/**
 * Picks the local repository for a task, in order of precedence:
 * 1. a `repo:<name>` label on the task,
 * 2. the first repo whose `match.labels` / `match.components` intersects the task's,
 * 3. the project's `default` repo, or its only repo.
 */
export function routeTask(task: Task, project: ProjectConfig | undefined): Route {
  if (!project) return { reason: `project ${task.project} is not in config` };
  const repos = Object.values(project.repos);
  const lower = (xs: string[] | undefined) => (xs ?? []).map((x) => x.toLowerCase());
  const labels = lower(task.labels);
  const components = lower(task.components);

  const explicit = labels.find((l) => l.startsWith(REPO_LABEL_PREFIX))?.slice(REPO_LABEL_PREFIX.length);
  if (explicit) {
    const repo = repos.find((r) => r.name.toLowerCase() === explicit);
    return repo
      ? { repo, reason: `label ${REPO_LABEL_PREFIX}${explicit}` }
      : { reason: `label ${REPO_LABEL_PREFIX}${explicit} names an unknown repo (known: ${repos.map((r) => r.name).join(", ")})` };
  }

  for (const repo of repos) {
    const label = lower(repo.match?.labels).find((l) => labels.includes(l));
    if (label) return { repo, reason: `label ${label}` };
    const component = lower(repo.match?.components).find((c) => components.includes(c));
    if (component) return { repo, reason: `component ${component}` };
  }

  if (project.default) return { repo: project.repos[project.default], reason: "default" };
  if (repos.length === 1) return { repo: repos[0], reason: "only repo" };
  return {
    reason: `no repo matched; add a ${REPO_LABEL_PREFIX}<name> label (${repos.map((r) => r.name).join(", ")}) or set "default"`,
  };
}
