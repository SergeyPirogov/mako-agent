# Task card redesign — mockups (#272)

Static HTML mockups of the task card. Open any file in a browser. Each one has a fixed size: the desktop screens are 1040 px wide and the phone screens are 390 px wide.

| File | State |
|---|---|
| [1-new-task.html](1-new-task.html) | New or snoozed task: description first, then the full estimate with the questions |
| [2-in-progress.html](2-in-progress.html) | The agent is working: "Now" block, changes panel |
| [3-mr-review.html](3-mr-review.html) | MR waits for the merge: agent review table (remark → answer → verdict), CI checks |
| [4-blocked.html](4-blocked.html) | Blocked by MR conflicts: what blocks the merge, the fixing action |
| [5-done.html](5-done.html) | Merged: summary, time split, assessment after, project lesson; no side panel |
| [6-phone-mr.html](6-phone-mr.html) | Phone, MR waiting for the merge |
| [7-agent-tab.html](7-agent-tab.html) | "Agent" tab: shared header, pinned plan, folded steps, permission banner |
| [8-phone-agent.html](8-phone-agent.html) | Phone, "Agent" tab |
| [9-agent-mr-tab.html](9-agent-mr-tab.html) | "Agent" tab with the MR panel: failed CI with its log, review comments, merge and close |

What's common to every screen:

- **Header.** Tracker icon · number · status pill, the title, and one muted meta line. The main action for the state sits on the right, and the rest go in "⋯".
- **Stage strip.** Estimate → Plan → Work → Review → CI/MR → Merge. A done stage is green ✓, the current one is highlighted, a failed one is red ✕ and a skipped one has a dashed circle. Each stage shows its time and cost.
- **Side panel.** Changes / Commits / MR appear only when there is a worktree or an MR, and they look the same on both tabs.
- **Colors** come from `go/internal/web/style.css` (light theme). The dark theme follows the same tokens.
- **Text** is in English only (see #274–#276). The task content in the mockups is sample data.
