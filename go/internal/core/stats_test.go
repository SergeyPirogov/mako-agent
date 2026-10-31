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
	}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	if st.Taken != 8 || st.Succeeded != 3 || st.Merged != 1 || st.Failed != 4 || st.Running != 1 {
		t.Fatalf("counts %+v", st)
	}
	if st.AvgSeconds != 2*3600 || st.MedianSeconds != 5400 || st.ReviewSeconds != 2*3600 {
		t.Fatalf("avg %v, median %v, review %v", st.AvgSeconds, st.MedianSeconds, st.ReviewSeconds)
	}
	if st.Retried != 1 || st.LastWeek != 3 {
		t.Fatalf("retried %d, last week %d", st.Retried, st.LastWeek)
	}
	wantProjects := []Group{{"g/app", 5, 2, 2, 4}, {"g/web", 3, 1, 2, 0}}
	wantExecutors := []Group{{"mako", 4, 2, 2, 4}, {"orca", 3, 1, 1, 0}, {"—", 1, 0, 1, 0}}
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
	want := []Reason{{"MR закрито без мерджу", 2}, {"Скасовано", 1}, {"не вдалось клонувати", 1}}
	if len(st.Reasons) != len(want) {
		t.Fatalf("reasons %+v", st.Reasons)
	}
	for i := range want {
		if st.Reasons[i] != want[i] {
			t.Fatalf("reasons %+v", st.Reasons)
		}
	}
}
