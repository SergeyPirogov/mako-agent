package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// cardView: what the task card shows on top: the main action for the task's state and its stages, from the
// estimate to the merge. The board draws them; which one is which is decided here, where tests can reach it.
type cardView struct {
	// Primary: the main action: "take", "restore", "push", "cancel", "stop", "resume", "focus", "fix-conflicts",
	// "fix-ci", "open-mr", "merge", "retry", "archive", "unarchive"; "" none (the card's «⋯» has the rest).
	Primary string      `json:"primary"`
	Stages  []stageView `json:"stages"`
}

// stageView: one stage of the strip. State: "done", "now", "fail", "skip" or "todo". Note: a word on it when there
// is no time or cost to show ("not needed", "conflicts"…).
type stageView struct {
	Key     string  `json:"key"`
	State   string  `json:"state"`
	Note    string  `json:"note,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
}

// The stages of the strip, in its order.
const (
	stEstimate = "estimate"
	stPlan     = "plan"
	stWork     = "work"
	stReview   = "review"
	stCI       = "ci"
	stMerge    = "merge"
)

func cardOf(v recordView, now time.Time) cardView {
	return cardView{Primary: primaryAction(v), Stages: stagesOf(v, now)}
}

// blockedBy: what holds the MR up, from Blocked: "conflicts", "ci" or "".
func blockedBy(v recordView) string {
	switch {
	case strings.Contains(v.Blocked, "conflicts"):
		return "conflicts"
	case strings.Contains(v.Blocked, "CI failed"):
		return "ci"
	}
	return ""
}

func primaryAction(v recordView) string {
	r := v.Record
	terminal := r.Executor == "terminal"
	switch {
	case r.ArchivedAt != 0:
		return "unarchive"
	case v.Column == "new" && v.Postponed:
		return "restore"
	case v.Column == "new":
		return "take"
	case r.Status == core.Queued && v.Starting != nil:
		return "cancel"
	case r.Status == core.Queued:
		return "push"
	case v.Column == "doing" && terminal && r.Worktree != "":
		if v.Live {
			return "stop"
		}
		return "resume"
	case v.Column == "doing" && r.Worktree != "":
		return "focus"
	case v.Column == "doing":
		// Orca's agent with no worktree yet: nothing to show, «⋯» has the rest.
		return ""
	case v.Column == "review":
		b := blockedBy(v)
		switch {
		// Only Mako's own running agent hears a request to fix (AskAgent); a stopped one is started first.
		case b != "" && terminal && r.Worktree != "" && v.Live:
			return "fix-" + b
		case b != "" && terminal && r.Worktree != "":
			return "resume"
		// Orca's agent is asked in Orca; CI still running: nothing to merge yet.
		case b != "" || v.CIWait != "":
			return "open-mr"
		}
		return "merge"
	case v.Column == "failed":
		return "retry"
	}
	return "archive"
}

func stagesOf(v recordView, now time.Time) []stageView {
	r := v.Record
	sec := r.StageSeconds(now)
	cost := func(phase string) float64 {
		if r.Metrics == nil {
			return 0
		}
		return r.Metrics.Cost[phase]
	}
	col := v.Column
	taken := col != "new"
	merged := r.ReviewState == "merged"
	finished := col == "done" || merged
	hasMR := r.PRURL != ""
	failed := r.Status == core.Failed || r.Status == core.Cancelled

	// Estimate.
	est := stageView{Key: stEstimate, State: "todo", Cost: cost(core.PhaseEstimate)}
	switch e := r.Estimate; {
	case e == nil && taken:
		est.State, est.Note = "skip", "none"
	case e == nil:
	case e.State == "running":
		est.State = "now"
	case e.State == "failed" && e.Size == "":
		est.State, est.Note = "fail", "failed"
		if taken {
			est.State = "skip"
		}
	default:
		est.State = "done"
		if col == "new" && e.Questions != "" {
			est.Note = questionsNote(e.Questions)
		}
	}

	// Plan.
	plan := stageView{Key: stPlan, State: "todo", Cost: cost(core.PhasePlan)}
	switch {
	case r.Planning && col == "doing":
		plan.State = "now"
	case r.Planning || r.PlanFirst || plan.Cost > 0:
		if taken && r.Status != core.Queued {
			plan.State = "done"
		} else {
			plan.Note = "planned"
		}
	case taken && r.Status != core.Queued:
		plan.State, plan.Note = "skip", "not needed"
	case r.Estimate != nil && r.Estimate.Plan:
		plan.Note = "advised"
	}

	// Work: the agent working and waiting for you, up to its MR.
	work := stageView{Key: stWork, State: "todo", Seconds: sec[core.StageAgent] + sec[core.StageYou], Cost: cost(core.PhaseWork)}
	switch {
	case r.Status == core.Queued:
		work.State, work.Note = "now", "queued"
	case col == "doing" && r.Planning:
		work.State = "todo"
	case col == "doing":
		work.State = "now"
	case failed:
		work.State = "fail"
	case hasMR || finished:
		work.State = "done"
	}

	// The reviewer agent.
	rev := stageView{Key: stReview, State: "todo", Cost: cost(core.PhaseReview)}
	if n := len(r.AgentReviews); n > 0 {
		last := r.AgentReviews[n-1]
		if n > 1 {
			rev.Note = fmt.Sprintf("%d rounds", n)
		}
		switch last.State {
		case "running":
			rev.State = "now"
		case "clean":
			rev.State = "done"
		case "failed":
			rev.State = "fail"
		case "skipped":
			rev.State, rev.Note = "skip", "skipped"
		case "notes":
			rev.State = "now"
			if !last.Sent && len(last.Remarks) > 0 {
				rev.Note = fmt.Sprintf("%d %s", len(last.Remarks), plural(len(last.Remarks), "remark", "remarks"))
			}
			if finished {
				rev.State = "done"
			}
		}
	} else if v.ReviewStep == "running" {
		rev.State = "now"
	} else if finished || col == "failed" {
		rev.State, rev.Note = "skip", "skipped"
	}

	// CI: the checks before the MR, then the MR's own CI.
	ci := stageView{Key: stCI, State: "todo", Seconds: sec[core.StageCI]}
	switch {
	case finished && hasMR:
		ci.State = "done"
	case col == "review" && blockedBy(v) == "conflicts":
		ci.State, ci.Note = "fail", "conflicts"
	case col == "review" && blockedBy(v) == "ci":
		ci.State, ci.Note = "fail", "CI failed"
	case col == "review" && v.CIWait != "":
		ci.State, ci.Note = "now", "running"
	case col == "review":
		ci.State = "done"
		if ci.Seconds < 1 {
			ci.Note = "green"
		}
	case r.Checks != nil && r.Checks.State == "failed":
		ci.State, ci.Note = "fail", "checks failed"
	case r.Checks != nil && r.Checks.State == "running" && col == "doing":
		ci.State, ci.Note = "now", "checks"
	}

	// The merge: the MR waits for you.
	merge := stageView{Key: stMerge, State: "todo", Seconds: sec[core.StageReview]}
	switch {
	case merged:
		merge.State = "done"
	case r.ReviewState == "closed":
		merge.State, merge.Note = "fail", "closed"
	case col == "review" && blockedBy(v) == "" && v.CIWait == "":
		merge.State = "now"
		if merge.Seconds < 60 {
			merge.Note = "waiting for you"
		}
	case finished && !hasMR:
		merge.State, merge.Note = "skip", "no MR"
	}

	// One stage is the current one: the merge waits while the reviewer's round is still on.
	if rev.State == "now" && merge.State == "now" {
		merge.State, merge.Note = "todo", ""
	}
	return []stageView{est, plan, work, rev, ci, merge}
}

// questionsNote: «3 questions» for the estimate's Markdown points.
func questionsNote(q string) string {
	n := 0
	for _, line := range strings.Split(q, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "• ") || len(t) > 2 && t[0] >= '0' && t[0] <= '9' && (t[1] == '.' || t[1] == ')') {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s", n, plural(n, "question", "questions"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
