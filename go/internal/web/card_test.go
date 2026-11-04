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
		stages  string // the states of estimate, plan, work, checks (CI and the review), merge
	}{
		{"new", view(core.Record{Status: core.Asking, Estimate: est}, nil), "take", "done todo todo todo todo"},
		{"new, not estimated", view(core.Record{Status: core.Asking}, nil), "take", "todo todo todo todo todo"},
		{"postponed", view(core.Record{Status: core.Snoozed, PostponedByYou: true, SnoozedUntil: now.Add(time.Hour).UnixMilli()}, nil), "restore", "todo todo todo todo todo"},
		{"queued", view(core.Record{Status: core.Queued, Estimate: est}, nil), "push", "done todo now todo todo"},
		{"queued, plans first", view(core.Record{Status: core.Queued, Estimate: est}, func(v *recordView) { v.WillPlan = true }), "push", "done now todo todo todo"},
		{"starting", view(core.Record{Status: core.Queued}, func(v *recordView) { v.Starting = &dispatch.StartProgress{} }), "cancel", "skip todo now todo todo"},
		{"working, live", view(term, func(v *recordView) { v.Live = true }), "stop", "skip skip now todo todo"},
		{"working, stopped", view(term, nil), "resume", "skip skip now todo todo"},
		{"planning", view(func() core.Record { r := term; r.Planning = true; return r }(), func(v *recordView) { v.Live = true }), "stop", "skip now todo todo todo"},
		{"in Orca", view(core.Record{Status: core.HandedOff, Worktree: "/wt"}, nil), "focus", "skip skip now todo todo"},
		{"in Orca, no worktree", view(core.Record{Status: core.HandedOff}, nil), "", "skip skip now todo todo"},
		{"checks failed", view(func() core.Record { r := term; r.Checks = &core.Checks{State: "failed"}; return r }(), nil), "resume", "skip skip now fail todo"},
		{"MR ready", view(mr, nil), "merge", "skip skip done done now"},
		{"MR, checked and ready", view(mr, func(v *recordView) { v.MergeReady = true }), "merge", "skip skip done done now"},
		{"MR, CI running", view(mr, func(v *recordView) { v.CIWait = "CI is running" }), "open-mr", "skip skip done now todo"},
		{"MR conflicts", view(mr, func(v *recordView) { v.Blocked, v.Live = "the MR has conflicts", true }), "fix-conflicts", "skip skip done fail todo"},
		{"MR CI failed", view(mr, func(v *recordView) { v.Blocked, v.Live = "CI failed", true }), "fix-ci", "skip skip done fail todo"},
		{"MR conflicts, agent not running", view(mr, func(v *recordView) { v.Blocked = "the MR has conflicts" }), "resume", "skip skip done fail todo"},
		{"MR conflicts in Orca", view(func() core.Record { r := mr; r.Executor = ""; return r }(), func(v *recordView) { v.Blocked = "the MR has conflicts" }), "open-mr", "skip skip done fail todo"},
		{"MR CI failed in Orca", view(func() core.Record { r := mr; r.Executor = ""; return r }(), func(v *recordView) { v.Blocked = "CI failed" }), "open-mr", "skip skip done fail todo"},
		{"MR blocked, agent stopped", view(mr, func(v *recordView) { v.Blocked = "agent stopped: CI failed, continue it in the chat" }), "resume", "skip skip done fail todo"},
		{"MR reviewed", view(func() core.Record {
			r := mr
			r.PlanFirst, r.AgentReviews = true, []core.AgentReview{{State: "notes", Sent: true}, {State: "clean"}}
			return r
		}(), nil), "merge", "skip done done done now"},
		{"MR, remarks being fixed", view(func() core.Record {
			r := mr
			r.AgentReviews = []core.AgentReview{{State: "notes", Sent: true}}
			return r
		}(), nil), "merge", "skip skip done now todo"},
		{"MR, agent judging the review need", view(func() core.Record {
			r := mr
			r.Assessment = &core.Assessment{State: "running", At: time.Now().UTC().Format(time.RFC3339Nano)}
			return r
		}(), nil), "open-mr", "skip skip done done now"},
		{"MR, stale judging does not hold the merge", view(func() core.Record {
			r := mr
			r.Assessment = &core.Assessment{State: "running", At: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)}
			return r
		}(), nil), "merge", "skip skip done done now"},
		{"MR, reviewer running", view(mr, func(v *recordView) { v.ReviewStep = "running" }), "open-mr", "skip skip done now todo"},
		{"MR, check it yourself left", view(mr, func(v *recordView) { v.Self = &dispatch.SelfCheckView{Required: true, Left: 1, Blocks: true} }), "open-mr", "skip skip done done now"},
		{"MR, checked yourself", view(mr, func(v *recordView) { v.Self = &dispatch.SelfCheckView{Required: true, Done: true} }), "merge", "skip skip done done now"},
		{"MR, agent fixing the remarks", view(func() core.Record {
			r := mr
			r.AgentReviews = []core.AgentReview{{State: "notes", Sent: true}}
			return r
		}(), func(v *recordView) { v.ReviewStep = "fixing" }), "open-mr", "skip skip done now todo"},
		{"merged", view(func() core.Record { r := mr; r.ReviewState = "merged"; return r }(), nil), "archive", "skip skip done done done"},
		{"MR closed", view(func() core.Record { r := mr; r.ReviewState = "closed"; return r }(), nil), "retry", "skip skip done todo fail"},
		{"failed", view(core.Record{Status: core.Failed, Estimate: est}, nil), "retry", "done skip fail todo todo"},
		{"archived", view(core.Record{Status: core.Done, ArchivedAt: 1}, nil), "unarchive", "skip skip done todo skip"},
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
	if got["work"].Seconds != 660 || got["work"].Cost != 3 || got["estimate"].Cost != 0.2 || got["checks"].Cost != 1 ||
		got["checks"].Seconds != 120 || got["merge"].Seconds != 300 {
		t.Errorf("stage metrics: %+v", got)
	}
	// Each stage shows the route its phase ran with; the CI the route of the agent's fixes.
	r.Metrics.Routes = map[string]core.Route{core.PhaseWork: {Model: "opus", Effort: "high", Why: "big, escalated: CI red ×2"}, core.PhaseCIFix: {Model: "sonnet", Effort: "medium"}}
	got = map[string]stageView{}
	for _, s := range stagesOf(recordView{Record: r, Column: r.Column(now.UnixMilli())}, now) {
		got[s.Key] = s
	}
	if got["work"].Route != "opus · high" || got["work"].Why != "big, escalated: CI red ×2" || got["checks"].Route != "CI fix: sonnet · medium" || got["plan"].Route != "" {
		t.Errorf("stage routes: %+v", got)
	}
	n := recordView{Record: core.Record{Status: core.Asking, Estimate: &core.Estimate{State: "done", Questions: "1. a?\n2. b?\n   more of b\n3) c?"}}, Column: "new"}
	if s := stagesOf(n, now)[0]; s.Note != "3 questions" {
		t.Errorf("estimate note %q", s.Note)
	}
}

func TestCardSkippedStageReasons(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	stage := func(r core.Record, key string) stageView {
		for _, s := range stagesOf(recordView{Record: r, Column: "doing"}, now) {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("no stage %q", key)
		return stageView{}
	}
	for _, c := range []struct {
		name, note, why string
		r               core.Record
	}{
		{"no plan", "from a plan", "The task was made from a written plan", core.Record{Status: core.HandedOff, NoPlan: true}},
		{"advised", "not picked", "The estimate advised a plan, but «plan first» was not picked", core.Record{Status: core.HandedOff, Estimate: &core.Estimate{State: "done", Size: "big", Plan: true}}},
		{"not needed", "not needed", "The estimate found no plan needed", core.Record{Status: core.HandedOff, Estimate: &core.Estimate{State: "done", Size: "small"}}},
		{"not needed, advice", "not needed", "The estimate found no plan needed: a one-line fix", core.Record{Status: core.HandedOff, Estimate: &core.Estimate{State: "done", Size: "small", Advice: "a one-line fix"}}},
		{"no estimate", "not picked", "No estimate, plan first was not picked", core.Record{Status: core.HandedOff}},
	} {
		s := stage(c.r, "plan")
		if s.State != "skip" || s.Note != c.note || s.Why != c.why {
			t.Errorf("%s: %+v", c.name, s)
		}
	}
	if s := stage(core.Record{Status: core.HandedOff}, "estimate"); s.State != "skip" || s.Note != "none" || s.Why != "Task was taken before it was estimated" {
		t.Errorf("no estimate: %+v", s)
	}
	if s := stage(core.Record{Status: core.HandedOff, Estimate: &core.Estimate{State: "failed", Error: "timeout"}}, "estimate"); s.State != "skip" || s.Note != "failed" || s.Why != "Estimate failed: timeout" {
		t.Errorf("failed estimate: %+v", s)
	}
}

// The Checks stage after a reviewer's round with remarks: «fixed» once the agent answered them, «fixing remarks» while
// it works, «N remarks» before «Виправити», «review running» only while the reviewer reads.
func TestCardChecksAfterRemarks(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	mr := core.Record{Executor: "terminal", Worktree: "/wt", Status: core.Done, PRURL: "https://github.com/o/r/pull/1",
		StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	remarks := []core.Remark{{}, {}}
	cases := []struct {
		name         string
		reviews      []core.AgentReview
		step         string
		state, merge string
		note         string // in the Checks note's review line
		not          string // and not there
		merged       bool   // the MR is merged
	}{
		{"answered", []core.AgentReview{{State: "notes", Sent: true, ReplyAt: "t"}}, "", "done", "now", "review ✓ fixed", "running", false},
		{"answered, two rounds", []core.AgentReview{{State: "clean"}, {State: "notes", Sent: true, ReplyAt: "t"}}, "", "done", "now", "review ✓ 2 rounds, fixed", "running", false},
		{"unverified", []core.AgentReview{{State: "notes", Sent: true, ReplyAt: "t", Unverified: true}}, "", "fail", "now", "review: unverified fix", "review failed", false},
		{"being fixed", []core.AgentReview{{State: "notes", Sent: true}}, "fixing", "now", "todo", "fixing remarks", "review running", false},
		{"not sent", []core.AgentReview{{State: "notes", Remarks: remarks}}, "notes", "now", "todo", "2 remarks", "review running", false},
		{"running", []core.AgentReview{{State: "running"}}, "running", "now", "todo", "review running", "", false},
		{"answered, merged", []core.AgentReview{{State: "notes", Sent: true, ReplyAt: "t"}}, "", "done", "done", "review ✓ fixed", "running", true},
		{"unverified, merged", []core.AgentReview{{State: "notes", Sent: true, ReplyAt: "t", Unverified: true}}, "", "done", "done", "review ✓", "unverified", true},
	}
	for _, c := range cases {
		r := mr
		r.AgentReviews = c.reviews
		v := recordView{Record: r, Column: "review", ReviewStep: c.step}
		if c.merged {
			v.ReviewState, v.Column = "merged", "done"
		}
		got := map[string]stageView{}
		for _, s := range stagesOf(v, now) {
			got[s.Key] = s
		}
		ch, m := got["checks"], got["merge"]
		if ch.State != c.state || m.State != c.merge {
			t.Errorf("%s: checks %q merge %q, want %q %q", c.name, ch.State, m.State, c.state, c.merge)
		}
		if !strings.Contains(ch.Note, c.note) || c.not != "" && strings.Contains(ch.Note, c.not) {
			t.Errorf("%s: note %q, want %q without %q", c.name, ch.Note, c.note, c.not)
		}
	}
}

// The Checks stage names what failed: the checks before the MR are not CI.
func TestChecksStageNote(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	note := func(v recordView) string {
		t.Helper()
		for _, s := range stagesOf(v, now) {
			if s.Key == "checks" {
				return s.Note
			}
		}
		t.Fatal("no checks stage")
		return ""
	}
	local := core.Record{Executor: "terminal", Worktree: "/wt", Status: core.Failed, Error: "checks failed 3 times: go test ./... exit 1",
		Checks: &core.Checks{State: "failed", Fails: 3}}
	if n := note(recordView{Record: local, Column: local.Column(now.UnixMilli())}); !strings.HasPrefix(n, "checks failed\n") {
		t.Errorf("failed local checks: note %q, want it to start with \"checks failed\"", n)
	}
	mr := core.Record{Executor: "terminal", Worktree: "/wt", Status: core.Done, PRURL: "https://github.com/o/r/pull/1"}
	if n := note(recordView{Record: mr, Column: "review", Blocked: "CI failed"}); !strings.HasPrefix(n, "CI failed\n") {
		t.Errorf("failed MR CI: note %q, want it to start with \"CI failed\"", n)
	}
}
