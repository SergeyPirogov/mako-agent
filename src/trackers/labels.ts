import type { Outcome } from "../types.js";

/**
 * Labels used by GitHub and GitLab. Only `trigger` is needed: it selects the issues for the agent.
 * Progress labels are opt-in; by default the issue's labels are left alone.
 */
export interface LabelConfig {
  /** Issues with this label are offered to the agent. */
  trigger: string;
  inProgress?: string | null;
  done?: string | null;
  failed?: string | null;
}

export const defaultLabels: LabelConfig = { trigger: "claude-agent" };

export function labelChanges(labels: LabelConfig, phase: "start" | Outcome): { add: string[]; remove: string[] } {
  const only = (...xs: (string | null | undefined)[]) => xs.filter((x): x is string => !!x);
  // The trigger label is removed only when a progress-label workflow was configured.
  const workflow = Boolean(labels.inProgress || labels.done || labels.failed);
  switch (phase) {
    case "start":
      return { add: only(labels.inProgress), remove: [] };
    case "success":
      return { add: only(labels.done), remove: workflow ? only(labels.inProgress, labels.trigger) : [] };
    case "no-changes":
      return { add: [], remove: only(labels.inProgress) };
    case "failed":
      return { add: only(labels.failed), remove: only(labels.inProgress) };
  }
}
