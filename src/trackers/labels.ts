import type { Outcome } from "../types.js";

/** Label workflow shared by GitHub and GitLab, which have no Jira-like statuses. */
export interface LabelConfig {
  /** Issues with this label are offered to the agent. */
  trigger: string;
  inProgress?: string | null;
  done?: string | null;
  failed?: string | null;
}

export const defaultLabels: LabelConfig = {
  trigger: "claude-agent",
  inProgress: "agent:in-progress",
  done: "agent:done",
  failed: "agent:failed",
};

export function labelChanges(labels: LabelConfig, phase: "start" | Outcome): { add: string[]; remove: string[] } {
  const only = (...xs: (string | null | undefined)[]) => xs.filter((x): x is string => !!x);
  switch (phase) {
    case "start":
      return { add: only(labels.inProgress), remove: [] };
    case "success":
      return { add: only(labels.done), remove: only(labels.inProgress, labels.trigger) };
    case "no-changes":
      return { add: [], remove: only(labels.inProgress) };
    case "failed":
      return { add: only(labels.failed), remove: only(labels.inProgress) };
  }
}
