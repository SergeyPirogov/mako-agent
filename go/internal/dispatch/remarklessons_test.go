package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// reviewed: a task's record with one reviewer round of the remarks.
func reviewed(id string, remarks ...core.Remark) core.Record {
	return core.Record{ID: id, Task: core.Task{ID: id, TrackerID: "gitlab", Project: "grp/app"},
		AgentReviews: []core.AgentReview{{At: now(), State: "notes", Remarks: remarks}}}
}

func must(file, text string) core.Remark {
	return core.Remark{ID: "R1", Weight: core.Must, File: file, Text: text}
}

func should(file, text string) core.Remark {
	return core.Remark{ID: "R2", Weight: core.Should, File: file, Text: text}
}

func TestRemarkGroups(t *testing.T) {
	since := time.Now().AddDate(0, 0, -90)
	records := []core.Record{
		reviewed("grp/app#463", must("go/internal/dispatch/lessons.go", "No test covers the new branch in `AddRule`: add a test."),
			should("go/internal/web/board.html", "The button label is not translated.")),
		reviewed("grp/app#466", must("go/internal/web/web.go:120", "No tests cover the new handler; add tests."),
			should("go/internal/core/task.go", "Rename the field to something clearer.")),
		reviewed("grp/app#470", must("go/internal/writer/writer.go", "Add a test: no test covers LessonFromRemarks.")),
		reviewed("grp/app#471", should("go/internal/state/state.go", "Why sort twice here?")),
	}
	groups := remarkGroups(collectRemarks(records, since), 3)
	if len(groups) != 1 {
		t.Fatalf("groups %+v", groups)
	}
	g := groups[0]
	if strings.Join(g.tasks, ",") != "grp/app#463,grp/app#466,grp/app#470" || g.weight != 6 || !strings.Contains(strings.Join(g.key, " "), "test") {
		t.Fatalf("group %+v", g)
	}
	if ex := g.examples(3); len(ex) != 3 || !strings.HasPrefix(ex[0], "grp/app#463: ") {
		t.Fatalf("examples %q", ex)
	}

	// The threshold is the settings': four tasks asked, three found.
	if groups := remarkGroups(collectRemarks(records, since), 4); len(groups) != 0 {
		t.Fatalf("with 4 tasks asked: %+v", groups)
	}
	// Remarks unlike each other make no group.
	unique := []core.Record{
		reviewed("grp/app#1", must("a.go", "The button label is not translated.")),
		reviewed("grp/app#2", must("b.go", "Close the file after reading it.")),
		reviewed("grp/app#3", must("c.go", "The loop never ends when the list is empty.")),
	}
	if groups := remarkGroups(collectRemarks(unique, since), 3); len(groups) != 0 {
		t.Fatalf("unique remarks: %+v", groups)
	}
	// A remark a later round carried over counts once; an old round not at all.
	again := records[1]
	again.AgentReviews = append(again.AgentReviews, core.AgentReview{At: now(), Remarks: []core.Remark{{Weight: core.Must, Text: "No tests cover the new handler; add tests.", Again: true}}})
	old := reviewed("grp/app#100", must("x.go", "Add a test for this."))
	old.AgentReviews[0].At = time.Now().AddDate(0, 0, -200).Format(time.RFC3339Nano)
	if groups := remarkGroups(collectRemarks([]core.Record{records[0], again, records[2], old}, since), 3); len(groups) != 1 || len(groups[0].items) != 3 {
		t.Fatalf("with a carried-over remark and an old round: %+v", groups)
	}
	// Ukrainian word forms are one kind as well.
	uk := []core.Record{
		reviewed("grp/app#7", must("a.go", "Немає тесту на нову гілку.")),
		reviewed("grp/app#8", must("b.go", "Додай тести на нову функцію.")),
		reviewed("grp/app#9", should("c.go", "Бракує тестів на нові гілки.")),
	}
	if groups := remarkGroups(collectRemarks(uk, since), 3); len(groups) != 1 {
		t.Fatalf("Ukrainian forms: %+v", groups)
	}
}

func TestSuggestRepoLesson(t *testing.T) {
	d, _, _ := setup(t)
	cfg := *d.Config()
	cfg.StateDir = t.TempDir()
	d.Cfg = &cfg
	var repo string
	for _, r := range cfg.Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# App\n"), 0o644)
	for i, text := range []string{"No test covers the new branch: add a test.", "Add tests: no test covers the handler.", "No test covers the new code path: add a test."} {
		r := reviewed(fmt.Sprintf("grp/app#%d", 10+i), must("go/x.go", text))
		d.Store.Update(r.ID, func(x *core.Record) { *x = r })
	}

	old := LessonFromRemarks
	t.Cleanup(func() { LessonFromRemarks = old })
	calls, answer, seenMD := 0, "", ""
	LessonFromRemarks = func(ctx context.Context, dir string, remarks []string, md string) (string, error) {
		calls++
		seenMD = md
		if len(remarks) != 3 || !strings.HasPrefix(remarks[0], "grp/app#") || !strings.Contains(remarks[0], "[must] go/x.go: ") {
			t.Errorf("remarks %q", remarks)
		}
		return answer, nil
	}
	again := func() {
		d.mu.Lock()
		d.repoLessoned = map[string]time.Time{}
		d.mu.Unlock()
		d.suggestRepoLesson("grp/app#10")
	}

	// The model finds no rule: nothing is offered, and the group is not asked about again.
	d.suggestRepoLesson("grp/app#10")
	if calls != 1 || d.LessonOffers()["grp/app"] != 0 || !strings.Contains(seenMD, "# App") {
		t.Fatalf("calls %d, offers %v, CLAUDE.md %q", calls, d.LessonOffers(), seenMD)
	}
	again()
	if calls != 1 {
		t.Fatalf("asked again about a group with no rule: %d", calls)
	}

	// With the «none» forgotten the model offers a rule; once an hour at most.
	d.updateLessons(func([]core.SavedLesson) []core.SavedLesson { return nil })
	answer = "Add a test for every new code path."
	again()
	if calls != 2 || d.LessonOffers()["grp/app"] != 1 {
		t.Fatalf("calls %d, offers %v", calls, d.LessonOffers())
	}
	d.updateLessons(func([]core.SavedLesson) []core.SavedLesson { return nil })
	d.suggestRepoLesson("grp/app#10")
	if calls != 2 {
		t.Fatal("looked again within the hour")
	}

	// One offer waits: no other is made; dismissed, the group is not offered again.
	again()
	if calls != 3 || len(d.Lessons()) != 1 {
		t.Fatalf("calls %d, lessons %+v", calls, d.Lessons())
	}
	l := d.Lessons()[0]
	if l.State != core.LessonOffered || l.Repo == "" || len(l.Tasks) != 3 || len(l.Examples) != 3 || l.ID == "" {
		t.Fatalf("offer %+v", l)
	}
	again()
	if calls != 3 {
		t.Fatal("a second offer while one waits")
	}
	if err := d.AnswerRepoLesson(context.Background(), l.ID, "no", ""); err != nil {
		t.Fatal(err)
	}
	if ls := d.Lessons(); ls[0].State != core.LessonDeclined || d.LessonOffers()["grp/app"] != 0 {
		t.Fatalf("after «Dismiss»: %+v", ls)
	}
	r := reviewed("grp/app#13", must("go/y.go", "No test for the new branch, add a test."))
	d.Store.Update(r.ID, func(x *core.Record) { *x = r })
	again()
	if calls != 3 || d.LessonOffers()["grp/app"] != 0 {
		t.Fatalf("a dismissed group came back: calls %d", calls)
	}
}

func TestAnswerRepoLesson(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	d, _, _ := setup(t)
	ctx := context.Background()
	cfg := *d.Config()
	cfg.StateDir = t.TempDir()
	d.Cfg = &cfg
	var repo, name string
	for _, r := range cfg.Trackers[0].Projects["grp/app"].Repos {
		repo, name = r.Path, r.Name
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(origin), "init", "-q", "--bare", "-b", "main", origin)
	git(t, repo, "remote", "set-url", "origin", origin)
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# App\n"), 0o644)
	git(t, repo, "add", "CLAUDE.md")
	git(t, repo, "commit", "-q", "-m", "init")
	git(t, repo, "push", "-q", "origin", "main")

	offer := core.SavedLesson{ID: "abc", TrackerID: "gitlab", Project: "grp/app", Repo: name, State: core.LessonOffered, Source: "reviewer",
		Rule: "x", Key: "test", Tasks: []string{"grp/app#1", "grp/app#2", "grp/app#3"}}
	d.keepLesson(offer)
	if err := d.AnswerRepoLesson(ctx, "nope", "base", "y"); err == nil {
		t.Fatal("an unknown offer was answered")
	}
	if err := d.AnswerRepoLesson(ctx, "abc", "base", "  Add a test for every\n new code path. "); err != nil {
		t.Fatal(err)
	}
	if md := gitOut(t, origin, "show", "main:CLAUDE.md"); md != "# App\n\n"+LessonsHeading+"\n\n- Add a test for every new code path." {
		t.Fatalf("origin's CLAUDE.md %q", md)
	}
	if body := gitOut(t, origin, "log", "-1", "--format=%B", "main"); !strings.Contains(body, "grp/app#1, grp/app#2, grp/app#3") {
		t.Fatalf("commit %q", body)
	}
	l := d.Lessons()[0]
	if l.State != "" || l.Branch != "main" || l.Commit == "" || l.Rule != "Add a test for every new code path." {
		t.Fatalf("saved %+v", l)
	}
	if err := d.AnswerRepoLesson(ctx, "abc", "base", "again"); err == nil {
		t.Fatal("a saved offer was answered again")
	}

	// The same rule once more: CLAUDE.md has it, nothing is committed or pushed.
	head := gitOut(t, origin, "rev-parse", "main")
	d.keepLesson(core.SavedLesson{ID: "def", TrackerID: "gitlab", Project: "grp/app", Repo: name, State: core.LessonOffered, Key: "other"})
	if err := d.AnswerRepoLesson(ctx, "def", "base", "add a test for every new code path"); err != nil {
		t.Fatal(err)
	}
	if gitOut(t, origin, "rev-parse", "main") != head {
		t.Fatal("a rule CLAUDE.md has was committed again")
	}
}

func TestAddRuleKnown(t *testing.T) {
	md := "# Repo\n\n" + LessonsHeading + "\n\n- Run `gofmt -l .` before committing.\n"
	if got := AddRule(md, "run gofmt -l . before committing"); got != md {
		t.Fatalf("a rule CLAUDE.md has was added again: %q", got)
	}
}
