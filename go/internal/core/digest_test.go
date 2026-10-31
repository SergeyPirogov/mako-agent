package core

import (
	"testing"
	"time"
)

func TestBuildDigest(t *testing.T) {
	since := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	now := since.Add(12 * time.Hour)
	at := func(h int) string { return since.Add(time.Duration(h) * time.Hour).Format(time.RFC3339Nano) }
	task := func(title string) Task { return Task{Title: title} }
	d := BuildDigest([]Record{
		{ID: "a#1", Status: HandedOff, Task: task("агент питає")},
		{ID: "a#2", Status: HandedOff, Task: task("агент працює")},
		{ID: "a#3", Status: Snoozed, Task: task("нова")},
		{ID: "a#4", Status: Snoozed, PostponedByYou: true, SnoozedUntil: now.Add(time.Hour).UnixMilli(), Task: task("відкладена")},
		{ID: "a#5", Status: Done, PRURL: "https://mr/5", FinishedAt: at(3), Task: task("MR за ніч")},
		{ID: "a#6", Status: Done, PRURL: "https://mr/6", FinishedAt: at(-30), Task: task("старий MR")},
		{ID: "a#7", Status: Failed, Error: "впало", UpdatedAt: at(5), Task: task("впала")},
		{ID: "a#8", Status: Failed, Error: "давно", UpdatedAt: at(-5), Task: task("впала раніше")},
		{ID: "a#9", Status: Done, PRURL: "https://mr/9", ReviewState: "merged", MergedAt: at(7), ArchivedAt: 1, Task: task("змерджено")},
		{ID: "a#10", Status: Done, PRURL: "https://mr/10", ReviewState: "merged", MergedAt: at(-2), Task: task("змерджено вчора")},
		{ID: "a#11", Status: Done, PRURL: "https://mr/11", FinishedAt: at(1), ArchivedAt: 1, Task: task("в архіві")},
	}, map[string]string{"a#1": "дозволити Bash?"}, since, now)

	ids := func(xs []DigestItem) (out []string) {
		for _, x := range xs {
			out = append(out, x.ID)
		}
		return out
	}
	check := func(name string, got []DigestItem, want ...string) {
		t.Helper()
		g := ids(got)
		if len(g) != len(want) {
			t.Fatalf("%s: %v, want %v", name, g, want)
		}
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("%s: %v, want %v", name, g, want)
			}
		}
	}
	check("waiting", d.Waiting, "a#1", "a#3")
	check("review", d.Review, "a#5", "a#6") // the newest first
	check("failed", d.Failed, "a#7")
	check("merged", d.Merged, "a#9")
	if d.Waiting[0].Note != "дозволити Bash?" || !d.Review[0].New || d.Review[1].New || d.Failed[0].Note != "впало" {
		t.Fatalf("details %+v", d)
	}
	if d.Count() != 6 || d.Line() != "чекає на вас: 2 · MR до мерджу: 2 · не вдалося: 1 · змерджено: 1" {
		t.Fatalf("count %d, line %q", d.Count(), d.Line())
	}
	if empty := BuildDigest(nil, nil, since, now); empty.Count() != 0 || empty.Line() != "" || empty.Waiting == nil {
		t.Fatalf("empty %+v", empty)
	}
}
