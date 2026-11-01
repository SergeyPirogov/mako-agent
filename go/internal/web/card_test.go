package web

import (
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
)

// The card's main action and its stages, by the task's state.
func TestCardByState(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	est := &core.Estimate{State: "done", Size: "big", Questions: "- one?\n- two?"}
	view := func(r core.Record, f func(*recordView)) recordView {
		v := recordView{Record: r, Column: r.Column(now.UnixMilli()), Postponed: r.Postponed(now.UnixMilli())}
		if f != nil {
			f(&v)
		}
		return v
	}
	term := core.Record{Executor: "terminal", Worktree: "/wt", Status: core.HandedOff, StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	mr := term
	mr.Status, mr.PRURL = core.Done, "https://github.com/o/r/pull/1"
	cases := []struct {
		name    string
		v       recordView
		primary string
		stages  string // the states of estimate, plan, work, review, ci, merge
	}{
		{"new", view(core.Record{Status: core.Asking, Estimate: est}, nil), "take", "done todo todo todo todo todo"},
		{"new, not estimated", view(core.Record{Status: core.Asking}, nil), "take", "todo todo todo todo todo todo"},
		{"postponed", view(core.Record{Status: core.Snoozed, PostponedByYou: true, SnoozedUntil: now.Add(time.Hour).UnixMilli()}, nil), "restore", "todo todo todo todo todo todo"},
		{"queued", view(core.Record{Status: core.Queued, Estimate: est}, nil), "push", "done todo now todo todo todo"},
		{"starting", view(core.Record{Status: core.Queued}, func(v *recordView) { v.Starting = &dispatch.StartProgress{} }), "cancel", "skip todo now todo todo todo"},
		{"working, live", view(term, func(v *recordView) { v.Live = true }), "stop", "skip skip now todo todo todo"},
		{"working, stopped", view(term, nil), "resume", "skip skip now todo todo todo"},
		{"planning", view(func() core.Record { r := term; r.Planning = true; return r }(), func(v *recordView) { v.Live = true }), "stop", "skip now todo todo todo todo"},
		{"in Orca", view(core.Record{Status: core.HandedOff, Worktree: "/wt"}, nil), "focus", "skip skip now todo todo todo"},
		{"checks failed", view(func() core.Record { r := term; r.Checks = &core.Checks{State: "failed"}; return r }(), nil), "resume", "skip skip now todo fail todo"},
		{"MR ready", view(mr, nil), "merge", "skip skip done todo done now"},
		{"MR, CI running", view(mr, func(v *recordView) { v.CIWait = "CI is running" }), "merge", "skip skip done todo now now"},
		{"MR conflicts", view(mr, func(v *recordView) { v.Blocked = "the MR has conflicts" }), "fix-conflicts", "skip skip done todo fail todo"},
		{"MR CI failed", view(mr, func(v *recordView) { v.Blocked = "CI failed" }), "fix-ci", "skip skip done todo fail todo"},
		{"MR blocked, agent stopped", view(mr, func(v *recordView) { v.Blocked = "agent stopped: CI failed, continue it in the chat" }), "resume", "skip skip done todo fail todo"},
		{"MR reviewed", view(func() core.Record {
			r := mr
			r.PlanFirst, r.AgentReviews = true, []core.AgentReview{{State: "notes", Sent: true}, {State: "clean"}}
			return r
		}(), nil), "merge", "skip done done done done now"},
		{"merged", view(func() core.Record { r := mr; r.ReviewState = "merged"; return r }(), nil), "archive", "skip skip done skip done done"},
		{"MR closed", view(func() core.Record { r := mr; r.ReviewState = "closed"; return r }(), nil), "retry", "skip skip done skip todo fail"},
		{"failed", view(core.Record{Status: core.Failed, Estimate: est}, nil), "retry", "done skip fail skip todo todo"},
		{"archived", view(core.Record{Status: core.Done, ArchivedAt: 1}, nil), "unarchive", "skip skip done skip todo skip"},
	}
	for _, c := range cases {
		got := cardOf(c.v, now)
		if got.Primary != c.primary {
			t.Errorf("%s: primary %q, want %q", c.name, got.Primary, c.primary)
		}
		var states []string
		for _, s := range got.Stages {
			states = append(states, s.State)
		}
		if s := strings.Join(states, " "); s != c.stages {
			t.Errorf("%s: stages %q, want %q", c.name, s, c.stages)
		}
	}
}

// Each stage carries its time and cost from the record's metrics; the estimate counts its questions.
func TestCardStageMetrics(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	r := core.Record{Status: core.Done, PRURL: "https://github.com/o/r/pull/1", Estimate: &core.Estimate{State: "done", Size: "small"},
		Metrics: &core.Metrics{
			Seconds: map[string]float64{core.StageAgent: 600, core.StageYou: 60, core.StageCI: 120, core.StageReview: 300},
			Cost:    map[string]float64{core.PhaseEstimate: 0.2, core.PhaseWork: 3, core.PhaseReview: 1},
		}}
	v := recordView{Record: r, Column: r.Column(now.UnixMilli())}
	got := map[string]stageView{}
	for _, s := range stagesOf(v, now) {
		got[s.Key] = s
	}
	if got["work"].Seconds != 660 || got["work"].Cost != 3 || got["estimate"].Cost != 0.2 || got["review"].Cost != 1 ||
		got["ci"].Seconds != 120 || got["merge"].Seconds != 300 {
		t.Errorf("stage metrics: %+v", got)
	}
	n := recordView{Record: core.Record{Status: core.Asking, Estimate: &core.Estimate{State: "done", Questions: "1. a?\n2. b?\n   more of b\n3) c?"}}, Column: "new"}
	if s := stagesOf(n, now)[0]; s.Note != "3 questions" {
		t.Errorf("estimate note %q", s.Note)
	}
}
