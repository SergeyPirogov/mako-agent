package core

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	app, web := Task{Project: "g/app"}, Task{Project: "g/web"}
	st := Summarize([]Record{
		{Status: Snoozed}, // never taken
		{Status: Cancelled, Error: "Задачу закрито в трекері"}, // taken from the board before a hand-off: not counted
		{Status: Done, Task: app, Executor: "mako", StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:30:00Z", PRURL: "u", Cost: 1.5},
		{Status: Done, Task: app, Executor: "mako", Attempt: 2, StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T11:30:00Z", PRURL: "u", ReviewState: "merged", MergedAt: "2026-10-01T13:30:00Z", Cost: 0.5},
		{Status: Done, Task: web, Executor: "orca", StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T14:00:00Z", PRURL: "u"},
		{Status: Failed, Task: web, Executor: "orca", StartedAt: "2026-10-01T10:00:00Z", ReviewState: "closed", Error: "MR закрито без мерджу"},
		{Status: Failed, Task: web, Error: "не вдалось клонувати\nfatal: ..."},
		{Status: Failed, Task: app, Executor: "mako", StartedAt: "2026-10-02T10:00:00Z", Error: "MR закрито без мерджу"},
		{Status: Cancelled, Task: app, Executor: "mako", StartedAt: "2026-10-02T10:00:00Z", Error: "Скасовано", Cost: 2},
		{Status: HandedOff, Task: app, Executor: "orca", StartedAt: "2026-10-03T10:00:00Z"},
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	if st.Taken != 8 || st.Succeeded != 3 || st.Merged != 1 || st.Failed != 4 || st.Running != 1 {
		t.Fatalf("counts %+v", st)
	}
	if st.AvgSeconds != 2*3600 || st.MedianSeconds != 5400 || st.ReviewSeconds != 2*3600 {
		t.Fatalf("avg %v, median %v, review %v", st.AvgSeconds, st.MedianSeconds, st.ReviewSeconds)
	}
	if st.Retried != 1 || st.LastWeek != 3 {
		t.Fatalf("retried %d, last week %d", st.Retried, st.LastWeek)
	}
	wantProjects := []Group{{"g/app", 5, 2, 2, 4, 0, 0, 0, 0}, {"g/web", 3, 1, 2, 0, 0, 0, 0, 0}}
	wantExecutors := []Group{{"mako", 4, 2, 2, 4, 0, 0, 0, 0}, {"orca", 3, 1, 1, 0, 0, 0, 0, 0}, {"—", 1, 0, 1, 0, 0, 0, 0, 0}}
	for _, c := range []struct{ got, want []Group }{{st.Projects, wantProjects}, {st.Executors, wantExecutors}} {
		if len(c.got) != len(c.want) {
			t.Fatalf("groups %+v", c.got)
		}
		for i := range c.want {
			if c.got[i] != c.want[i] {
				t.Fatalf("groups %+v", c.got)
			}
		}
	}
	if st.Cost != 4 || st.Priced != 3 {
		t.Fatalf("cost %v over %d", st.Cost, st.Priced)
	}
	want := []Reason{{"MR закрито без мерджу", 2}, {"Cancelled", 1}, {"не вдалось клонувати", 1}}
	if len(st.Reasons) != len(want) {
		t.Fatalf("reasons %+v", st.Reasons)
	}
	for i := range want {
		if st.Reasons[i] != want[i] {
			t.Fatalf("reasons %+v", st.Reasons)
		}
	}
}

func TestSummarizeAssessments(t *testing.T) {
	rated := func(project string, clarity, complexity, completeness, risk int) Record {
		return Record{Status: Done, Task: Task{Project: project}, StartedAt: "2026-10-01T10:00:00Z", PRURL: "u",
			Assessment: &Assessment{State: "done", Clarity: Score{Value: clarity}, Complexity: Score{Value: complexity},
				Completeness: Score{Value: completeness}, Risk: Score{Value: risk}}}
	}
	running := rated("g/app", 1, 1, 1, 1)
	running.Assessment.State = "running"
	st := Summarize([]Record{
		rated("g/app", 2, 3, 5, 1), rated("g/app", 4, 1, 3, 3), rated("g/web", 5, 5, 4, 2), running,
		{Status: Done, Task: Task{Project: "g/web"}, StartedAt: "2026-10-01T10:00:00Z", PRURL: "u"},
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	if st.Assessed != 3 || st.Clarity != 11.0/3 || st.Complexity != 3 || st.Completeness != 4 || st.Risk != 2 {
		t.Fatalf("assessment %+v", st)
	}
	for _, g := range st.Projects {
		if g.Name == "g/app" && (g.Assessed != 2 || g.Clarity != 3) || g.Name == "g/web" && (g.Assessed != 1 || g.Clarity != 5) {
			t.Fatalf("groups %+v", st.Projects)
		}
	}
}

func TestSummarizeForecast(t *testing.T) {
	task := func(expected, came string, rec Record) Record {
		rec.Task.Project = "g/app"
		if rec.StartedAt == "" {
			rec.StartedAt = "2026-10-01T10:00:00Z"
		}
		if expected != "" {
			rec.Estimate = &Estimate{State: "done", Size: expected}
		}
		if came != "" {
			rec.Assessment = &Assessment{State: "done", Size: came, Clarity: Score{Value: 3}}
		}
		return rec
	}
	merged := Record{Status: Done, ReviewState: "merged", FinishedAt: "2026-10-01T11:00:00Z", Cost: 1}
	redone := merged
	redone.Attempt = 2
	fixed := merged
	fixed.ReviewFixes = 1
	noted := merged
	noted.AgentReviews = []AgentReview{{State: "clean"}, {State: "notes"}}
	failed := Record{Status: Failed, Error: "x", Cost: 3}
	inReview := Record{Status: Done, FinishedAt: "2026-10-01T13:00:00Z"}
	failedEstimate := task("small", "small", merged)
	failedEstimate.Estimate.State = "failed"
	st := Summarize([]Record{
		task("small", "small", merged),   // hit, clean
		task("small", "medium", redone),  // miss, rework: another attempt
		task("small", "", fixed),         // no final size: not compared; rework: review comments
		task("medium", "big", noted),     // miss, rework: the reviewer agent's notes
		task("medium", "medium", merged), // hit, clean
		task("big", "big", failed),       // hit, failed
		task("big", "big", inReview),     // hit, outcome not known yet
		task("", "big", merged),          // no estimate: in neither
		failedEstimate,                   // the estimate failed: in neither
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	f := st.Forecast
	if f.Compared != 6 || f.Hits != 4 {
		t.Fatalf("forecast %+v", f)
	}
	want := [][]int{{1, 1, 0}, {0, 1, 1}, {0, 0, 2}}
	for i := range want {
		for j := range want[i] {
			if f.Matrix[i][j] != want[i][j] {
				t.Fatalf("matrix %v", f.Matrix)
			}
		}
	}
	wantSizes := []SizeGroup{
		{Size: "small", Taken: 3, Ended: 3, Clean: 1, Seconds: 3600, Timed: 3, Cost: 1, Priced: 3},
		{Size: "medium", Taken: 2, Ended: 2, Clean: 1, Seconds: 3600, Timed: 2, Cost: 1, Priced: 2},
		{Size: "big", Taken: 2, Ended: 1, Clean: 0, Seconds: 3 * 3600, Timed: 1, Cost: 3, Priced: 1},
	}
	if len(st.BySize) != len(wantSizes) {
		t.Fatalf("by size %+v", st.BySize)
	}
	for i := range wantSizes {
		if st.BySize[i] != wantSizes[i] {
			t.Fatalf("by size %d: %+v, want %+v", i, st.BySize[i], wantSizes[i])
		}
	}
}

func TestSummarizeUnclearByAuthor(t *testing.T) {
	rated := func(project, author string, clarity int) Record {
		return Record{Status: Done, Task: Task{Project: project, Author: author}, StartedAt: "2026-10-01T10:00:00Z",
			Assessment: &Assessment{State: "done", Clarity: Score{Value: clarity}}}
	}
	st := Summarize([]Record{
		rated("g/app", "ann", 1), rated("g/app", "ann", 2), rated("g/app", "bob", 5), rated("g/web", "bob", 3),
		rated("g/web", "", 1), // no author: in the project, not in the authors
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	want := []Group{
		{Name: "ann", Taken: 2, Succeeded: 2, Clarity: 1.5, Assessed: 2, Unclear: 2},
		{Name: "bob", Taken: 2, Succeeded: 2, Clarity: 4, Assessed: 2, Unclear: 0},
	}
	if len(st.Authors) != len(want) || st.Authors[0] != want[0] || st.Authors[1] != want[1] {
		t.Fatalf("authors %+v", st.Authors)
	}
	for _, g := range st.Projects {
		if g.Name == "g/app" && g.Unclear != 2 || g.Name == "g/web" && g.Unclear != 1 {
			t.Fatalf("projects %+v", st.Projects)
		}
	}
}

func TestSummarizeSince(t *testing.T) {
	st := Summarize([]Record{
		{Status: Done, StartedAt: "2026-09-01T10:00:00Z", Estimate: &Estimate{State: "done", Size: "small"}},
		{Status: Done, StartedAt: "2026-10-05T10:00:00Z", Estimate: &Estimate{State: "done", Size: "big"}},
		{Status: Failed, UpdatedAt: "2026-10-06T10:00:00Z", Error: "x"}, // never handed off: by when it changed
		{Status: Failed, UpdatedAt: "2026-09-06T10:00:00Z", Error: "y"},
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if st.Taken != 2 || st.Succeeded != 1 || st.Failed != 1 || st.BySize[0].Taken != 0 || st.BySize[2].Taken != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestSummarizeByModel(t *testing.T) {
	st := Summarize([]Record{
		{Status: Done, AgentModel: "claude-opus-4-7", StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:30:00Z", Cost: 3},
		{Status: Failed, AgentModel: "claude-opus-4-7", StartedAt: "2026-10-01T10:00:00Z", Error: "x", Cost: 1},
		{Status: Done, AgentModel: "opus", StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:30:00Z"},
		{Status: Done, AgentModel: "sonnet", StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:30:00Z", Cost: 0.5},
		{Status: HandedOff, StartedAt: "2026-10-02T10:00:00Z"},
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	want := []Group{{"opus", 3, 2, 1, 4, 0, 0, 0, 0}, {"sonnet", 1, 1, 0, 0.5, 0, 0, 0, 0}, {"—", 1, 0, 0, 0, 0, 0, 0, 0}}
	if len(st.Models) != len(want) {
		t.Fatalf("models %+v", st.Models)
	}
	for i := range want {
		if st.Models[i] != want[i] {
			t.Fatalf("models %+v", st.Models)
		}
	}
}

func TestModelFamily(t *testing.T) {
	for in, want := range map[string]string{"claude-opus-4-7[1m]": "opus", "Sonnet": "sonnet", "claude-haiku-4-5-20251001": "haiku", "claude-x-1": "x-1", "": "—"} {
		if got := ModelFamily(in); got != want {
			t.Fatalf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestSummarizeFlow(t *testing.T) {
	st := Summarize([]Record{
		{Status: Done, StartedAt: "2026-10-07T10:00:00Z", FinishedAt: "2026-10-07T10:30:00Z", ReviewState: "merged", MergedAt: "2026-10-07T12:00:00Z", Cost: 1},
		{Status: Done, StartedAt: "2026-10-07T11:00:00Z", FinishedAt: "2026-10-07T11:30:00Z", ReviewState: "merged", MergedAt: "2026-10-07T15:00:00Z", ReviewFixes: 1},
		{Status: Done, StartedAt: "2026-10-08T09:00:00Z", FinishedAt: "2026-10-08T09:30:00Z", PRURL: "u"}, // on review
		{Status: Cancelled, StartedAt: "2026-10-08T09:00:00Z", Error: "Скасовано", Cost: 0.5},
		{Status: Failed, StartedAt: "2026-10-08T09:00:00Z", Error: "Session cancelled by the developer."},
		{Status: Failed, StartedAt: "2026-09-01T09:00:00Z", Error: "впало"}, // before the trend
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	if st.Ended != 5 || st.Clean != 1 || st.Cancelled != 2 || st.LeadSeconds != 3*3600 {
		t.Fatalf("ended %d, clean %d, cancelled %d, lead %v", st.Ended, st.Clean, st.Cancelled, st.LeadSeconds)
	}
	if st.Reasons[0] != (Reason{"Cancelled", 2}) {
		t.Fatalf("reasons %+v", st.Reasons)
	}
	if len(st.Days) != 14 || st.Days[0].Date != "2026-09-25" {
		t.Fatalf("days %+v", st.Days)
	}
	if got := st.Days[12]; got != (Day{"2026-10-07", 2, 2, 0, 1}) {
		t.Fatalf("7th %+v", got)
	}
	if got := st.Days[13]; got != (Day{"2026-10-08", 3, 0, 2, 0.5}) {
		t.Fatalf("8th %+v", got)
	}
	week := Summarize(nil, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if len(week.Days) != 7 || week.Days[6].Date != "2026-10-08" {
		t.Fatalf("week %+v", week.Days)
	}
}

// A cancel from the board is written as "Cancelled" now, "Скасовано" in older records; both count as cancelled, not failed.
func TestSummarizeCancelledBothSpellings(t *testing.T) {
	app := Task{Project: "g/app"}
	st := Summarize([]Record{
		{Status: Cancelled, Task: app, Executor: "mako", StartedAt: "2026-10-02T10:00:00Z", Error: "Cancelled"},
		{Status: Cancelled, Task: app, Executor: "mako", StartedAt: "2026-10-02T11:00:00Z", Error: "Скасовано"},
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Time{})
	if st.Cancelled != 2 {
		t.Fatalf("cancelled %d", st.Cancelled)
	}
	if len(st.Reasons) != 1 || st.Reasons[0] != (Reason{"Cancelled", 2}) {
		t.Fatalf("reasons %+v", st.Reasons)
	}
}
