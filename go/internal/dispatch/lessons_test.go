package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestAddRule(t *testing.T) {
	h := LessonsHeading
	cases := []struct{ md, want string }{
		{"", h + "\n\n- B\n"},
		{"# Repo\n\nText.\n", "# Repo\n\nText.\n\n" + h + "\n\n- B\n"},
		{"# Repo\n\n" + h + "\n\n- A\n", "# Repo\n\n" + h + "\n\n- A\n- B\n"},
		{"# Repo\n\n" + h + "\n\n- A\n\n## Tests\n\nRun go test.\n", "# Repo\n\n" + h + "\n\n- A\n- B\n\n## Tests\n\nRun go test.\n"},
		{h + "\n## Next\n", h + "\n\n- B\n\n## Next\n"},
		// A CLAUDE.md with the older Ukrainian heading keeps adding to it.
		{"# Repo\n\n" + LegacyLessonsHeading + "\n\n- A\n", "# Repo\n\n" + LegacyLessonsHeading + "\n\n- A\n- B\n"},
	}
	for _, c := range cases {
		if got := AddRule(c.md, "B"); got != c.want {
			t.Errorf("AddRule(%q) = %q, want %q", c.md, got, c.want)
		}
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLessons(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	d, _, _ := setup(t)
	ctx := context.Background()
	cfg := *d.Config()
	cfg.StateDir = t.TempDir()
	d.Cfg = &cfg
	var repo string
	for _, r := range cfg.Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	// The clone with an origin of its own, and the task's worktree on its branch.
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(origin), "init", "-q", "--bare", "-b", "main", origin)
	git(t, repo, "remote", "set-url", "origin", origin)
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# App\n\nGo.\n"), 0o644)
	git(t, repo, "add", "CLAUDE.md")
	git(t, repo, "commit", "-q", "-m", "init")
	git(t, repo, "push", "-q", "origin", "main")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "fix/5-pay", wt, "main")
	os.WriteFile(filepath.Join(wt, "work.go"), []byte("package app\n"), 0o644)
	git(t, wt, "add", "work.go") // the agent's own work, staged: it stays out of the lesson's commit

	d.Poll(ctx)
	const id = "grp/app#5"
	d.Store.Update(id, func(r *core.Record) { r.Worktree, r.Branch, r.Executor = wt, "fix/5-pay", terminalExecutor })

	old := LessonFrom
	t.Cleanup(func() { LessonFrom = old })
	var asked string
	LessonFrom = func(ctx context.Context, dir, message, md string) (string, error) {
		asked = md
		if strings.Contains(message, "board.html") {
			return "Не чіпай стилі в board.html: вони в style.css", nil
		}
		return "", nil
	}
	d.suggestLesson(id, "Продовжуй")
	d.suggestLesson(id, "Запусти тести ще раз, будь ласка")
	if r, _ := d.Store.Get(id); r.Lesson != nil {
		t.Fatalf("no lesson in these: %+v", r.Lesson)
	}
	d.suggestLesson(id, "не роби так у board.html, стилі там не місце")
	r, _ := d.Store.Get(id)
	if r.Lesson == nil || r.Lesson.Rule != "Не чіпай стилі в board.html: вони в style.css" || !strings.Contains(asked, "# App") {
		t.Fatalf("lesson %+v, CLAUDE.md seen %q", r.Lesson, asked)
	}

	// «Зберегти в гілку задачі»: the rule as you edited it, one commit with CLAUDE.md alone.
	if err := d.SaveLesson(ctx, id, "branch", "  Стилі — лише в style.css,\n не в board.html "); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "CLAUDE.md")); string(b) != "# App\n\nGo.\n\n"+LessonsHeading+"\n\n- Стилі — лише в style.css, не в board.html\n" {
		t.Fatalf("CLAUDE.md %q", b)
	}
	if s := gitOut(t, wt, "log", "-1", "--format=%s"); s != "grp/app#5: Додати урок з рев'ю в CLAUDE.md" {
		t.Fatalf("commit %q", s)
	}
	if files := gitOut(t, wt, "show", "--name-only", "--format=", "HEAD"); files != "CLAUDE.md" {
		t.Fatalf("committed %q", files)
	}
	if st := gitOut(t, wt, "status", "--porcelain"); st != "A  work.go" {
		t.Fatalf("status %q", st)
	}
	if r, _ := d.Store.Get(id); r.Lesson != nil {
		t.Fatal("the offer stays after saving")
	}

	// «Зберегти в main»: committed on top of origin/main and pushed; the clone's checkout is left alone.
	d.Store.Update(id, func(r *core.Record) { r.Lesson = &core.Lesson{Rule: "x"} })
	if err := d.SaveLesson(ctx, id, "base", "Тести — в _test.go поруч"); err != nil {
		t.Fatal(err)
	}
	if md := gitOut(t, origin, "show", "main:CLAUDE.md"); !strings.HasSuffix(md, LessonsHeading+"\n\n- Тести — в _test.go поруч") {
		t.Fatalf("origin's CLAUDE.md %q", md)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md")); string(b) != "# App\n\nGo.\n" {
		t.Fatalf("the clone changed: %q", b)
	}
	if wts := gitOut(t, repo, "worktree", "list"); strings.Count(wts, "\n") != 1 {
		t.Fatalf("worktrees left: %s", wts)
	}

	ls := d.Lessons()
	if len(ls) != 2 || ls[0].Branch != "main" || ls[1].Branch != "fix/5-pay" || ls[0].Project != "grp/app" || ls[0].TrackerID != "gitlab" || ls[0].Commit == "" {
		t.Fatalf("saved %+v", ls)
	}

	// «Ні»: the offer goes, nothing is written.
	d.Store.Update(id, func(r *core.Record) { r.Lesson = &core.Lesson{Rule: "y"} })
	if err := d.SaveLesson(ctx, id, "no", ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get(id); r.Lesson != nil || len(d.Lessons()) != 2 {
		t.Fatalf("after «Ні»: %+v, %d", r.Lesson, len(d.Lessons()))
	}
}
