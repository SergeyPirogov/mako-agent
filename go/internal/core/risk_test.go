package core

import (
	"testing"
	"time"
)

// Patterns as in .gitignore: «**» for any folders, a name without «/» at any depth, a folder by its trailing «/».
func TestMatchPath(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"migrations/**", "migrations/001_init.sql", true},
		{"migrations/**", "migrations/2026/002.sql", true},
		{"migrations/**", "db/migrations/001.sql", false},
		{"migrations/**", "migrations", false},
		{"**/auth/**", "internal/auth/token.go", true},
		{"**/auth/**", "auth/token.go", true},
		{"**/auth/**", "internal/oauth/token.go", false},
		{"*.env*", ".env", true},
		{"*.env*", "config/.env.local", true},
		{"*.env*", "envelope.go", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{"/.github/", ".github/workflows/ci.yml", true},
		{"go.mod", "go/go.mod", true},
		{"go/go.mod", "go.mod", false},
		{"internal/*/risk.go", "internal/dispatch/risk.go", true},
		{"internal/*/risk.go", "internal/a/b/risk.go", false},
		{"**/*.sql", "a/b/c.sql", true},
		{"", "a.go", false},
		{"[", "[", false}, // a broken pattern matches nothing
	} {
		if got := MatchPath(c.pattern, c.path); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// The level is the highest of the diff's signals and the agent's rating; an older assessment without Review counts
// by its risk alone; one not done does not count.
func TestRiskOf(t *testing.T) {
	assessed := func(risk int, review *ReviewAdvice) *Assessment {
		return &Assessment{State: "done", Risk: Score{Value: risk, Why: "touches the merge"}, Review: review}
	}
	look := func(levels ...string) *RiskLook {
		l := &RiskLook{Commit: "abc"}
		for _, lv := range levels {
			l.Signals = append(l.Signals, RiskSignal{Kind: SignalPath, Level: lv, Text: lv})
		}
		return l
	}
	for _, c := range []struct {
		name    string
		r       Record
		want    string
		signals int
	}{
		{"nothing known", Record{}, "", 0},
		{"no signals, low rating", Record{Risk: look(), Assessment: assessed(2, &ReviewAdvice{Need: true})}, RiskLow, 1},
		{"rating 3", Record{Assessment: assessed(3, nil)}, RiskMedium, 1},
		{"old assessment without Review, risk 4", Record{Assessment: assessed(4, nil)}, RiskHigh, 1},
		{"signal above the rating", Record{Risk: look(RiskMedium, RiskHigh), Assessment: assessed(1, nil)}, RiskHigh, 3},
		{"rating above the signals", Record{Risk: look(RiskMedium), Assessment: assessed(5, nil)}, RiskHigh, 2},
		{"block wins", Record{Risk: look(RiskBlock), Assessment: assessed(5, nil)}, RiskBlock, 2},
		{"signals only", Record{Risk: look(RiskMedium)}, RiskMedium, 1},
		{"assessment running", Record{Risk: look(), Assessment: &Assessment{State: "running", Risk: Score{Value: 5}}}, "", 0},
		{"assessment failed", Record{Assessment: &Assessment{State: "failed"}}, "", 0},
	} {
		level, signals := RiskOf(c.r)
		if level != c.want || len(signals) != c.signals {
			t.Errorf("%s: %q with %d signals, want %q with %d", c.name, level, len(signals), c.want, c.signals)
		}
	}
	_, signals := RiskOf(Record{Assessment: assessed(4, nil)})
	if s := signals[0]; s.Kind != SignalAssessment || s.Text != "Agent's assessment: risk 4 of 5: touches the merge" {
		t.Fatalf("the rating's signal: %+v", s)
	}
	if MaxRisk(RiskLow, "nonsense") != RiskLow || MaxRisk("", RiskLow) != RiskLow || RiskLevel("x") != "" {
		t.Fatal("MaxRisk / RiskLevel")
	}
	l := look(RiskMedium, RiskBlock)
	if b := l.Blocked(); len(b) != 1 || b[0].Level != RiskBlock || (*RiskLook)(nil).Blocked() != nil || (*RiskLook)(nil).Level() != "" {
		t.Fatalf("Blocked: %+v", b)
	}
}

// The stats by risk: the merged tasks by the level at the merge (an older one by its assessment), the auto-merges
// and which of them a later fix or revert names.
func TestSummarizeRiskLevels(t *testing.T) {
	at := func(h int) string { return time.Date(2026, 11, 1, h, 0, 0, 0, time.UTC).Format(time.RFC3339Nano) }
	merged := func(n, level, by string) Record {
		id := "o/r#" + n
		return Record{ID: id, Task: Task{ID: id, Project: "o/r"}, Status: Done, StartedAt: at(1), ReviewState: "merged", MergedAt: at(2),
			PRURL: "https://github.com/o/r/pull/" + n + "0", RiskAtMerge: level, MergedBy: by}
	}
	old := merged("6", "", "")
	old.Assessment = &Assessment{State: "done", Risk: Score{Value: 4}} // before the policy: its rating
	later := func(n, title string, labels ...string) Record {
		id := "o/r#" + n
		return Record{ID: id, Task: Task{ID: id, Project: "o/r", Title: title, Labels: labels}, Status: Queued, CreatedAt: at(5)}
	}
	records := []Record{
		merged("1", RiskLow, "auto"),
		merged("2", RiskLow, "auto"),
		merged("3", RiskLow, "board"),
		merged("4", RiskMedium, ""),
		merged("5", RiskHigh, "board"),
		old,
		merged("7", "", ""), // nothing known: not counted
		later("11", "Revert «#1»"),
		later("12", "Кнопка після #3 зламалась", "bug"),
		later("13", "Згадка #2 без мітки"),
		later("14", "Hotfix: pay twice after #2"), // no label: the title says it
		later("15", "Document the hotfix process, see #3"),
	}
	got := Summarize(records, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), time.Time{}).RiskLevels
	want := RiskStats{Merged: 6, Low: 3, Medium: 1, High: 2, AutoMerged: 2, AutoMergedFixed: 2}
	if got != want {
		t.Fatalf("%+v, want %+v", got, want)
	}
	for title, want := range map[string]bool{"Rollback the cart": true, "Revert «#1»": true, "[Hotfix] pay twice": true, "hotfix: pay": true,
		"Reverted colours are nice": false, "Document the hotfix process": false, "Fix the revert button": false} {
		if isRevert(Record{Task: Task{Title: title}}) != want {
			t.Errorf("isRevert(%q) = %v, want %v", title, !want, want)
		}
	}
	if !isRevert(Record{Task: Task{Labels: []string{"hotfix"}}}) || !isRevert(Record{Task: Task{Labels: []string{"revert"}}}) {
		t.Fatal("isRevert by label")
	}
}
