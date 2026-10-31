package core

import "testing"

func TestSummarize(t *testing.T) {
	st := Summarize([]Record{
		{Status: Snoozed}, // never taken
		{Status: Cancelled, Error: "Задачу закрито в трекері"}, // taken from the board before a hand-off: not counted
		{Status: Done, StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:30:00Z", PRURL: "u", Cost: 1.5},
		{Status: Done, StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T11:30:00Z", PRURL: "u", ReviewState: "merged", Cost: 0.5},
		{Status: Failed, StartedAt: "2026-10-01T10:00:00Z", ReviewState: "closed", Error: "MR закрито без мерджу"},
		{Status: Failed, Error: "не вдалось клонувати\nfatal: ..."},
		{Status: Failed, StartedAt: "2026-10-02T10:00:00Z", Error: "MR закрито без мерджу"},
		{Status: Cancelled, StartedAt: "2026-10-02T10:00:00Z", Error: "Скасовано", Cost: 2},
		{Status: HandedOff, StartedAt: "2026-10-03T10:00:00Z"},
	})
	if st.Taken != 7 || st.Succeeded != 2 || st.Merged != 1 || st.Failed != 4 || st.Running != 1 {
		t.Fatalf("counts %+v", st)
	}
	if st.AvgSeconds != 3600 {
		t.Fatalf("avg %v", st.AvgSeconds)
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
