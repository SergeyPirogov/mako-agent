package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestAddRuleUnder(t *testing.T) {
	md := "# App\n\n" + LegacyLessonsHeading + "\n\n- A\n"
	got := AddRuleUnder(md, NavHeading, "Дошка — web/board.html")
	if want := md + "\n" + NavHeading + "\n\n- Дошка — web/board.html\n"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if got = AddRuleUnder(got, NavHeading, "B"); !strings.HasSuffix(got, "- Дошка — web/board.html\n- B\n") || strings.Contains(got, "- A\n- B") {
		t.Fatalf("%q", got)
	}
}

// transcriptAt writes a Claude Code transcript of session in worktree where orca looks for it.
func transcriptAt(t *testing.T, claudeDir, worktree, session string, lines ...string) {
	dir := filepath.Join(claudeDir, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(worktree, "-"))
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNavRetro(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	d, _, _ := setup(t)
	ctx := context.Background()
	cfg := *d.Config()
	cfg.StateDir = t.TempDir()
	d.Cfg = &cfg
	var repo, repoName string
	for n, r := range cfg.Trackers[0].Projects["grp/app"].Repos {
		repo, repoName = r.Path, n
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(origin), "init", "-q", "--bare", "-b", "main", origin)
	git(t, repo, "remote", "set-url", "origin", origin)
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# App\n"), 0o644)
	git(t, repo, "add", "CLAUDE.md")
	git(t, repo, "commit", "-q", "-m", "init")
	git(t, repo, "push", "-q", "origin", "main")

	d.Poll(ctx)
	r5, _ := d.Store.Get("grp/app#5")
	task := r5.Task
	// Three merged tasks: one with its agent's transcript, one whose transcript is gone, one with no worktree; and
	// one not merged.
	wt := filepath.Join(t.TempDir(), "wt-5")
	merged := func(id, wt, session, at string) {
		tk := task
		tk.ID = id
		d.Store.Update(id, func(r *core.Record) {
			r.Task, r.Status, r.ReviewState, r.MergedAt, r.Worktree, r.SessionID = tk, core.Done, "merged", at, wt, session
		})
	}
	merged("grp/app#5", wt, "s-5", "2026-10-03T10:00:00Z")
	merged("grp/app#6", filepath.Join(t.TempDir(), "wt-6"), "s-6", "2026-10-02T10:00:00Z")
	merged("grp/app#7", "", "", "2026-10-01T10:00:00Z")
	d.Store.Update("grp/app#8", func(r *core.Record) { tk := task; tk.ID = "grp/app#8"; r.Task, r.Status = tk, core.Done })
	transcriptAt(t, claudeDir, wt, "s-5",
		fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"payButton","path":%q}}]}}`, wt),
		`{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"No matches found"}]}}`,
		fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m2","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"%s/web/board.html"}}]}}`, wt),
	)

	key := navKey(task.TrackerID, "grp/app", repoName)
	if got := d.NavRetros("grp/app"); len(got) != 1 || got[0].Key != key || got[0].Merged != 3 || got[0].State != "" {
		t.Fatalf("retros %+v", got)
	}
	if err := d.NavRetro(navKey(task.TrackerID, "grp/other", repoName)); err == nil {
		t.Fatal("a retro of a repository without merged tasks")
	}

	old := NavFrom
	t.Cleanup(func() { NavFrom = old })
	var digest, md string
	NavFrom = func(ctx context.Context, dir string, route core.Route, dg, claudeMD string) ([]core.NavSuggestion, error) {
		digest, md = dg, claudeMD
		return []core.NavSuggestion{
			{ID: "1", Kind: core.NavClaudeMD, Title: "Карта дошки", Evidence: "шукали payButton", Rule: "Дошка — web/board.html"},
			{ID: "2", Kind: core.NavRefactor, Title: "Розбити board.html", Evidence: "великий", Rule: "Винести stats"},
		}, nil
	}

	nr, err := d.startNavRetro(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.startNavRetro(key); err == nil {
		t.Fatal("a second retro while one runs")
	}
	if got := d.NavRetros("grp/app"); got[0].State != "running" {
		t.Fatalf("retros %+v", got)
	}
	d.navRetro(ctx, key, nr)
	got := d.NavRetros("grp/app")[0]
	if got.State != "done" || strings.Join(got.Tasks, ",") != "grp/app#5" || strings.Join(got.Skipped, ",") != "grp/app#6,grp/app#7" || len(got.Suggestions) != 2 {
		t.Fatalf("retro %+v", got)
	}
	if !strings.Contains(digest, "Tasks analyzed: 1 (the agent edited files in 1)") || !strings.Contains(digest, "Grep: payButton (1)") || md != "# App" {
		t.Fatalf("digest %q, CLAUDE.md %q", digest, md)
	}
	if logs := d.Logs("grp/app#6"); len(logs) == 0 || !strings.Contains(logs[len(logs)-1].Text, "transcript is not kept") {
		t.Fatalf("logs %+v", logs)
	}

	// «Зберегти в CLAUDE.md»: under its own heading on origin's main, pushed; a refactor is not saved there.
	if err := d.SaveNavRule(ctx, key, "2", "x"); err == nil {
		t.Fatal("a refactor saved to CLAUDE.md")
	}
	if err := d.SaveNavRule(ctx, key, "1", " Дошка — web/board.html,\n стилі в style.css "); err != nil {
		t.Fatal(err)
	}
	if s := gitOut(t, origin, "show", "main:CLAUDE.md"); s != "# App\n\n"+NavHeading+"\n\n- Дошка — web/board.html, стилі в style.css" {
		t.Fatalf("origin's CLAUDE.md %q", s)
	}
	if s := gitOut(t, origin, "log", "-1", "--format=%s", "main"); s != "Додати підказку з навігації в CLAUDE.md" {
		t.Fatalf("commit %q", s)
	}
	if ls := d.Lessons(); len(ls) != 1 || ls[0].Section != "navigation" || ls[0].Repo != repoName {
		t.Fatalf("lessons %+v", ls)
	}
	if err := d.MarkNav(key, "2", "task:grp/app#9"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkNav(key, "2", "maybe"); err == nil {
		t.Fatal("an unknown answer")
	}
	s := d.NavRetros("grp/app")[0].Suggestions
	if s[0].Done != "saved" || s[0].Rule != "Дошка — web/board.html, стилі в style.css" || s[1].Done != "task:grp/app#9" {
		t.Fatalf("suggestions %+v", s)
	}

	// Without Claude Code (as in tests) the model fails, and the retro says so.
	NavFrom = old
	nr, _ = d.startNavRetro(key)
	d.navRetro(ctx, key, nr)
	if got := d.NavRetros("grp/app")[0]; got.State != "failed" || got.Error == "" {
		t.Fatalf("retro %+v", got)
	}
}
