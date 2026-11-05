package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// fakeGitLab: one project grp/app with issues, statuses, notes and merge requests, enough for the dispatcher.
type fakeGitLab struct {
	mu        sync.Mutex
	issues    map[int]map[string]any
	mrs       []map[string]any
	notes     []string
	notesOf   map[int][]string // the notes of each issue, read back with the issue
	failNotes bool             // the tracker refuses new notes
	notesDown bool             // the tracker fails to give the notes
	statusSet []string
	assigned  []any
	unindexed map[int]bool // open, but the search does not find them yet
	base      string
	trace     string // every CI job's log
	// mergeRefused: the host refuses to merge the MR (a rule of the project, a race with another merge).
	mergeRefused bool
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	p := r.URL.Path
	switch {
	case p == "/api/v4/user":
		reply(map[string]any{"id": 7, "username": "serg"})
	case p == "/api/v4/merge_requests": // MRs to review: none
		reply([]any{})
	case strings.HasPrefix(p, "/api/v4/projects/grp/app/jobs/") && strings.HasSuffix(p, "/trace"):
		w.Write([]byte(f.trace))
	case p == "/api/graphql":
		var q struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(body, &q)
		if strings.Contains(q.Query, "workItemUpdate") {
			f.statusSet = append(f.statusSet, q.Variables["name"].(string))
			reply(map[string]any{"data": map[string]any{"workItemUpdate": map[string]any{"errors": []string{}}}})
			return
		}
		statuses := []map[string]string{{"name": "To do"}, {"name": "In progress"}, {"name": "Done"}}
		reply(map[string]any{"data": map[string]any{"project": map[string]any{"workItemTypes": map[string]any{"nodes": []any{
			map[string]any{"name": "Issue", "widgetDefinitions": []any{map[string]any{}, map[string]any{"allowedStatuses": statuses}}},
		}}}}})
	case (p == "/api/v4/projects/grp%2Fapp/issues" || r.URL.RawPath == "/api/v4/projects/grp%2Fapp/issues") && r.Method == "POST":
		var in map[string]any
		json.Unmarshal(body, &in)
		iid := 100 + len(f.issues)
		labels := []string{}
		if l, _ := in["labels"].(string); l != "" {
			labels = strings.Split(l, ",")
		}
		is := map[string]any{"id": 1000 + iid, "iid": iid, "state": "opened", "title": in["title"], "description": in["description"],
			"web_url": fmt.Sprintf("%s/grp/app/-/issues/%d", f.base, iid), "labels": labels, "issue_type": in["issue_type"],
			"assignees": []any{map[string]any{"id": 7, "username": "serg"}}, "references": map[string]string{"full": fmt.Sprintf("grp/app#%d", iid)}}
		f.issues[iid] = is
		reply(is)
	case p == "/api/v4/projects/grp%2Fapp/issues" || r.URL.RawPath == "/api/v4/projects/grp%2Fapp/issues":
		list := []any{}
		for _, is := range f.issues {
			if is["state"] == "opened" && !f.unindexed[is["iid"].(int)] {
				list = append(list, is)
			}
		}
		reply(list)
	case strings.HasPrefix(p, "/api/v4/projects/grp/app/issues/"):
		rest := strings.TrimPrefix(p, "/api/v4/projects/grp/app/issues/")
		var iid int
		fmt.Sscanf(rest, "%d", &iid)
		is := f.issues[iid]
		if is == nil {
			w.WriteHeader(404)
			reply(map[string]string{"message": "404 Not found"})
			return
		}
		switch {
		case strings.HasSuffix(rest, "/notes") && r.Method == "GET" && f.notesDown:
			w.WriteHeader(502)
			reply(map[string]string{"message": "502 Bad Gateway"})
		case strings.HasSuffix(rest, "/notes") && r.Method == "GET":
			list := []any{}
			for _, b := range f.notesOf[iid] {
				list = append(list, map[string]any{"body": b, "author": map[string]string{"name": "Serg"}})
			}
			reply(list)
		case strings.HasSuffix(rest, "/notes"):
			if f.failNotes {
				w.WriteHeader(403)
				reply(map[string]string{"message": "403 Forbidden"})
				return
			}
			var n map[string]string
			json.Unmarshal(body, &n)
			f.notes = append(f.notes, n["body"])
			if f.notesOf == nil {
				f.notesOf = map[int][]string{}
			}
			f.notesOf[iid] = append(f.notesOf[iid], n["body"])
			is["user_notes_count"] = len(f.notesOf[iid])
			reply(map[string]any{"id": len(f.notes)})
		case r.Method == "PUT":
			var ch map[string]any
			json.Unmarshal(body, &ch)
			if ids, ok := ch["assignee_ids"]; ok {
				f.assigned = append(f.assigned, ids)
			}
			if ch["state_event"] == "close" {
				is["state"] = "closed"
			}
			reply(is)
		default:
			reply(is)
		}
	case strings.HasPrefix(p, "/api/v4/projects/grp/app/merge_requests"):
		if r.URL.Query().Get("source_branch") != "" {
			list := []any{}
			for _, mr := range f.mrs {
				if mr["source_branch"] == r.URL.Query().Get("source_branch") {
					list = append(list, mr)
				}
			}
			reply(list)
			return
		}
		if r.Method == "POST" {
			var in map[string]any
			json.Unmarshal(body, &in)
			iid := len(f.mrs) + 1
			mr := map[string]any{"iid": iid, "state": "opened", "source_branch": in["source_branch"], "target_branch": in["target_branch"],
				"title": in["title"], "description": in["description"], "web_url": fmt.Sprintf("%s/grp/app/-/merge_requests/%d", f.base, iid)}
			f.mrs = append(f.mrs, mr)
			reply(mr)
			return
		}
		mr := f.mrs[0]
		if r.Method == "PUT" {
			var ch map[string]any
			json.Unmarshal(body, &ch)
			if strings.HasSuffix(p, "/merge") && f.mergeRefused {
				w.WriteHeader(405)
				reply(map[string]string{"message": "405 Method Not Allowed"})
				return
			}
			if strings.HasSuffix(p, "/merge") {
				mr["state"] = "merged"
			} else if ch["state_event"] == "close" {
				mr["state"] = "closed"
			} else {
				mr["title"], mr["description"] = ch["title"], ch["description"]
			}
		}
		reply(mr)
	default:
		w.WriteHeader(404)
		reply(map[string]string{"message": "no route " + p})
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestMain(m *testing.M) {
	// Once, before any test: the goroutines a test leaves behind read it.
	notify.Enabled = false
	trackers.RetryBase = time.Millisecond // a 5xx of a fake tracker is tried again at once
	// The tests' board is at 127.0.0.1:1, nobody answers there: the pre-push hook would stop every push the
	// tests make themselves. The hook's own tests turn it on.
	pushHooks = false
	i18n.SetAgentLanguage(i18n.UK) // the tests' tasks and what the agent writes are Ukrainian
	os.Exit(m.Run())
}

func setup(t *testing.T) (*Dispatcher, *fakeGitLab, *[]orca.Job) {
	// Never the real Claude Code from a test.
	old := writer.Claude
	writer.Claude = "mako-test-no-claude"
	t.Cleanup(func() { writer.Claude = old })
	f := &fakeGitLab{issues: map[int]map[string]any{}}
	srv := httptest.NewServer(http.StripPrefix("", f))
	t.Cleanup(srv.Close)
	f.base = srv.URL
	f.issues[5] = map[string]any{
		"id": 1005, "iid": 5, "state": "opened", "title": "Кнопка оплати не працює", "description": "Падає на кліку",
		"web_url": srv.URL + "/grp/app/-/issues/5", "labels": []string{"claude-agent"}, "issue_type": "issue",
		"assignees": []any{}, "references": map[string]string{"full": "grp/app#5"},
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "app")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.Index(host, ":")]
	git(t, repo, "remote", "add", "origin", "git@"+host+":grp/app.git")

	raw := fmt.Sprintf(`{"language": "uk", "wipLimit": 2, "executors": {"orca": {"type": "orca"}},
	  "trackers": [{"type": "gitlab", "baseUrl": %q, "projects": {"grp/app": {"repoPath": %q, "baseBranch": "main"}}}]}`, srv.URL, repo)
	cfg, err := config.Parse([]byte(raw), map[string]string{"GITLAB_TOKEN": "x"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home, cfg.StateDir = dir, filepath.Join(dir, ".mako")
	cfg.Estimate = false // the tests of the estimate turn it on
	store, err := state.Open(filepath.Join(dir, ".mako"))
	if err != nil {
		t.Fatal(err)
	}
	gl, err := trackers.New(cfg.Trackers[0], cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := New(cfg, store, []trackers.Tracker{gl})
	d.Term = &fakeTerminals{}
	d.LastMessage = func(string) string { return "" }
	jobs := &[]orca.Job{}
	var handing sync.Mutex // the tasks of one round are handed off at once
	d.Hand = func(ctx context.Context, o config.Orca, j orca.Job, log func(string)) (*orca.Result, error) {
		handing.Lock()
		defer handing.Unlock()
		*jobs = append(*jobs, j)
		return &orca.Result{Worktree: filepath.Join(dir, "wt"), Branch: j.Branch}, nil
	}
	os.MkdirAll(filepath.Join(dir, "wt"), 0o755)
	return d, f, jobs
}

func TestTaskLifecycle(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()

	d.Poll(ctx)
	r, ok := d.Store.Get("grp/app#5")
	if !ok || r.Column(time.Now().UnixMilli()) != "new" {
		t.Fatalf("new task not on the board: %+v", r)
	}

	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ = d.Store.Get("grp/app#5")
	if r.Status != core.HandedOff {
		t.Fatalf("status %s, error %q", r.Status, r.Error)
	}
	if len(*jobs) != 1 || (*jobs)[0].Branch != "fix/5-knopka-oplaty-ne-pratsyuye" {
		t.Fatalf("hand-off: %+v", *jobs)
	}
	if len(f.statusSet) != 1 || f.statusSet[0] != "In progress" {
		t.Fatalf("status at start: %v", f.statusSet)
	}
	if len(f.assigned) != 1 || len(f.notes) != 1 || f.notes[0] != "Взяв задачу в роботу. Гілка fix/5-knopka-oplaty-ne-pratsyuye. Як буде готово, відкрию MR." {
		t.Fatalf("assigned %v, notes %v", f.assigned, f.notes)
	}

	// No MR yet: still in Orca.
	d.CheckHandoffs(ctx)
	if r, _ = d.Store.Get("grp/app#5"); r.Status != core.HandedOff {
		t.Fatalf("moved without an MR: %s", r.Status)
	}

	// The agent opened the MR: review column, comment with the link.
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "opened", "web_url": f.base + "/grp/app/-/merge_requests/9",
		"source_branch": r.Branch, "title": "Fix the payment button", "description": "## What changed\n- handler fixed\n- test added for the click\n"})
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	r, _ = d.Store.Get("grp/app#5")
	if r.Column(time.Now().UnixMilli()) != "review" || !strings.HasSuffix(r.PRURL, "/merge_requests/9") {
		t.Fatalf("not in review: %+v", r)
	}
	if !strings.Contains(f.notes[len(f.notes)-1], "merge_requests/9") {
		t.Fatalf("no MR comment: %v", f.notes)
	}

	// Merged: Done, the issue closes, the card goes to «Готово».
	f.mu.Lock()
	f.mrs[0]["state"] = "merged"
	f.mu.Unlock()
	d.CheckMerges(ctx)
	r, _ = d.Store.Get("grp/app#5")
	if r.ReviewState != "merged" || r.Column(time.Now().UnixMilli()) != "done" {
		t.Fatalf("merge not seen: %+v", r)
	}
	if f.statusSet[len(f.statusSet)-1] != "Done" || f.issues[5]["state"] != "closed" {
		t.Fatalf("issue after merge: %v %v", f.statusSet, f.issues[5]["state"])
	}

	// Two days later it goes to the archive.
	if n := d.AutoArchive(time.Now().Add(49 * time.Hour)); n != 1 {
		t.Fatalf("archived %d", n)
	}
}

// A cause is written with its kind, so the statistics count it once whatever language the text is in; starting
// the task again clears it with the error.
func TestFailureKindIsWritten(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	id := "grp/app#5"
	d.Take(ctx, id)
	d.Wait()
	r, _ := d.Store.Get(id)
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "opened", "web_url": f.base + "/grp/app/-/merge_requests/9",
		"source_branch": r.Branch, "title": "Fix", "description": "## What changed\n- x\n"})
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	f.mu.Lock()
	f.mrs[0]["state"] = "closed"
	f.mu.Unlock()
	d.CheckMerges(ctx)
	r, _ = d.Store.Get(id)
	if r.Status != core.Failed || r.ErrorKind != core.KindMRClosed {
		t.Fatalf("closed MR: status %s, error %q, kind %q", r.Status, r.Error, r.ErrorKind)
	}

	d.Cancel(id)
	if r, _ = d.Store.Get(id); r.Status != core.Cancelled || r.ErrorKind != core.KindCancelled {
		t.Fatalf("cancel: status %s, error %q, kind %q", r.Status, r.Error, r.ErrorKind)
	}

	// Taken again, the task has neither the error nor its kind.
	d.Take(ctx, id)
	d.Wait()
	if r, _ = d.Store.Get(id); r.Error != "" || r.ErrorKind != "" {
		t.Fatalf("take: error %q, kind %q", r.Error, r.ErrorKind)
	}

	// Retried differently after a failure: the same.
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Error, r.ErrorKind = core.Failed, "MR closed without merging", core.KindMRClosed
	})
	d.RemoveWorkspace = func(context.Context, string, string, string) error { return nil }
	if err := d.Redo(ctx, id, "", ""); err != nil {
		t.Fatal(err)
	}
	d.Wait()
	if r, _ = d.Store.Get(id); r.Error != "" || r.ErrorKind != "" {
		t.Fatalf("redo: error %q, kind %q", r.Error, r.ErrorKind)
	}
}

// The issue goes to review only after CI: in work while it runs, «Заблоковані» on the board when it fails, and
// the tracker's review status with the comment once it passes.
func TestReviewWaitsForCI(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	id := "grp/app#5"
	d.Take(ctx, id)
	d.Wait()
	r, _ := d.Store.Get(id)
	notes := len(f.notes)
	pipeline := map[string]any{"id": 1, "status": "running", "web_url": "ci"}
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "opened", "web_url": f.base + "/grp/app/-/merge_requests/9", "source_branch": r.Branch,
		"title": "Fix the payment button", "description": strings.Repeat("written by a person ", 10), "head_pipeline": pipeline})
	f.mu.Unlock()

	d.CheckHandoffs(ctx)
	if r, _ = d.Store.Get(id); r.Status != core.HandedOff || r.Column(0) != "doing" || len(f.notes) != notes {
		t.Fatalf("in review while CI runs: %+v, notes %v", r, f.notes)
	}
	// The link is kept at once, and «чекаю на CI» is said once however many cycles pass.
	if want := f.base + "/grp/app/-/merge_requests/9"; r.PRURL != want {
		t.Fatalf("MR link not kept while CI runs: %q", r.PRURL)
	}
	d.Log(id, "щось між циклами")
	d.CheckHandoffs(ctx)
	waits := 0
	for _, l := range d.Logs(id) {
		if strings.Contains(l.Text, "waiting for CI") {
			waits++
		}
	}
	if r, _ = d.Store.Get(id); waits != 1 || r.Status != core.HandedOff || r.Column(0) != "doing" {
		t.Fatalf("waiting for CI: %d log lines, %+v", waits, r)
	}

	f.mu.Lock()
	pipeline["status"] = "failed"
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	r, _ = d.Store.Get(id)
	if r.Status != core.Done || !r.AwaitingCI || d.Blocked(r) != "CI failed" || len(f.notes) != notes {
		t.Fatalf("failed CI: %+v, blocked %q, notes %v", r, d.Blocked(r), f.notes)
	}
	d.CheckBlocked(ctx)
	if r, _ = d.Store.Get(id); !r.AwaitingCI || len(f.notes) != notes {
		t.Fatalf("moved to review with a failed CI: %+v", r)
	}

	f.mu.Lock()
	pipeline["status"] = "success"
	f.mu.Unlock()
	d.forgetReview(id)
	d.CheckBlocked(ctx)
	r, _ = d.Store.Get(id)
	if r.AwaitingCI || d.Blocked(r) != "" || len(f.notes) != notes+1 || !strings.Contains(f.notes[notes], "merge_requests/9") {
		t.Fatalf("CI passed, not in review: %+v, notes %v", r, f.notes)
	}
}

func TestClosedWithoutMergeFails(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "closed", "web_url": f.base + "/grp/app/-/merge_requests/9", "source_branch": r.Branch,
		"title": "x", "description": strings.Repeat("written by a person ", 10)})
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	r, _ = d.Store.Get("grp/app#5")
	if r.Column(time.Now().UnixMilli()) != "failed" || r.ReviewState != "closed" {
		t.Fatalf("closed MR: %+v", r)
	}
}

func TestOthersTasksAndGone(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	f.issues[6] = map[string]any{"id": 1006, "iid": 6, "state": "opened", "title": "Чужа", "web_url": "u", "labels": []string{},
		"assignees": []any{map[string]any{"id": 8, "username": "colleague"}}, "references": map[string]string{"full": "grp/app#6"}}
	f.issues[7] = map[string]any{"id": 1007, "iid": 7, "state": "opened", "title": "Спільна", "web_url": "u", "labels": []string{},
		"assignees": []any{map[string]any{"id": 8, "username": "colleague"}, map[string]any{"id": 7, "username": "serg"}}, "references": map[string]string{"full": "grp/app#7"}}
	d.Poll(ctx)
	if _, ok := d.Store.Get("grp/app#6"); ok {
		t.Fatal("a colleague's task was offered")
	}
	if _, ok := d.Store.Get("grp/app#7"); !ok {
		t.Fatal("a task with me among two assignees was not offered")
	}
	// Closed in GitLab before anyone took it: it leaves the search and is marked so.
	f.mu.Lock()
	f.issues[5]["state"] = "closed"
	f.mu.Unlock()
	d.Poll(ctx)
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.Cancelled || r.Gone != "closed" {
		t.Fatalf("closed issue: %+v", r)
	}
}

func TestWipLimit(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	for i := 10; i < 13; i++ {
		f.issues[i] = map[string]any{"id": 1000 + i, "iid": i, "state": "opened", "title": fmt.Sprintf("Task %d", i), "web_url": "u",
			"labels": []string{}, "assignees": []any{}, "references": map[string]string{"full": fmt.Sprintf("grp/app#%d", i)}}
	}
	d.Poll(ctx)
	for i := 10; i < 13; i++ {
		d.Take(ctx, fmt.Sprintf("grp/app#%d", i))
		d.Wait()
	}
	if len(*jobs) != 2 {
		t.Fatalf("WIP 2, handed %d", len(*jobs))
	}
	if r, _ := d.Store.Get("grp/app#12"); r.Status != core.Queued {
		t.Fatalf("third task: %s", r.Status)
	}
	// «Запустити зараз»: the queued one goes past the limit.
	d.Push(ctx, "grp/app#12")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#12"); r.Status != core.HandedOff || len(*jobs) != 3 {
		t.Fatalf("pushed task: %s, handed %d", r.Status, len(*jobs))
	}
	// Only a queued task is pushed.
	d.Push(ctx, "grp/app#12")
	d.Wait()
	if len(*jobs) != 3 {
		t.Fatalf("pushed twice: handed %d", len(*jobs))
	}
}

// A task put off comes back at its time with a notification; «Повернути» brings it back at once.
func TestLaterAndRemind(t *testing.T) {
	d, _, _ := setup(t)
	d.Poll(context.Background())
	id := "grp/app#5"
	d.Later(id, 60)
	if r, _ := d.Store.Get(id); !r.Postponed(time.Now().UnixMilli()) {
		t.Fatalf("not put off: %+v", r)
	}
	d.remind(time.Now())
	if r, _ := d.Store.Get(id); !r.Postponed(time.Now().UnixMilli()) {
		t.Fatal("came back before its time")
	}
	d.remind(time.Now().Add(61 * time.Minute))
	if r, _ := d.Store.Get(id); r.PostponedByYou || r.SnoozedUntil != 0 || r.Status != core.Snoozed {
		t.Fatalf("not back at its time: %+v", r)
	}
	d.Later(id, 60)
	d.Later(id, -1)
	if r, _ := d.Store.Get(id); r.Postponed(time.Now().UnixMilli()) {
		t.Fatal("«Повернути» left it put off")
	}
}

// «Повторити інакше» on a task in review: its MR closes, its workspace goes, and the task starts over on -try2
// with the note, an account of the last attempt and the chosen model.
func TestRedo(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	var removed []string
	d.RemoveWorkspace = func(ctx context.Context, repoPath, worktree, branch string) error {
		removed = append(removed, worktree+"@"+branch)
		return nil
	}
	d.Poll(ctx)
	id := "grp/app#5"
	d.Take(ctx, id)
	d.Wait()
	r, _ := d.Store.Get(id)
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "opened", "web_url": f.base + "/grp/app/-/merge_requests/9", "source_branch": r.Branch,
		"title": "Fix the payment button", "description": strings.Repeat("written by a person ", 10)})
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	d.Store.Update(id, func(r *core.Record) { r.Summary = "- changed the backend handler" })
	if r, _ = d.Store.Get(id); r.Column(time.Now().UnixMilli()) != "review" {
		t.Fatalf("not in review: %+v", r)
	}

	if err := d.Redo(ctx, id, "  Fix it on the frontend, leave the API alone  ", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := d.Redo(ctx, id, "again", ""); err == nil {
		t.Fatal("a second redo while the first one starts")
	}
	d.Wait()
	r, _ = d.Store.Get(id)
	if r.Status != core.HandedOff || r.Attempt != 2 || r.Branch != "fix/5-knopka-oplaty-ne-pratsyuye-try2" || r.PRURL != "" {
		t.Fatalf("not started over: %+v", r)
	}
	// A retry goes a level up; the model picked for it stays.
	if r.Escalation != 1 || len(r.EscalatedFor) != 1 || r.EscalatedFor[0] != "retry" || r.RouteOf(core.PhaseWork).Model != "opus" {
		t.Fatalf("retry escalation %d %v, route %+v", r.Escalation, r.EscalatedFor, r.RouteOf(core.PhaseWork))
	}
	if f.mrs[0]["state"] != "closed" {
		t.Fatalf("old MR left open: %v", f.mrs[0]["state"])
	}
	if len(removed) != 1 || !strings.HasSuffix(removed[0], "@fix/5-knopka-oplaty-ne-pratsyuye") {
		t.Fatalf("old workspace: %v", removed)
	}
	if len(*jobs) != 2 {
		t.Fatalf("hand-offs: %d", len(*jobs))
	}
	j := (*jobs)[1]
	if j.Model != "opus" || j.RetryNote != "Fix it on the frontend, leave the API alone" || j.LastAttempt != "- changed the backend handler" {
		t.Fatalf("new attempt's job: %+v", j)
	}
	if p := orca.FullPrompt(j, d.Config().Orca); !strings.Contains(p, "Fix it on the frontend") || !strings.Contains(p, "changed the backend handler") ||
		!strings.Contains(p, "fix/5-knopka-oplaty-ne-pratsyuye-try2") {
		t.Fatalf("prompt:\n%s", p)
	}

	// A new task cannot be redone.
	f.issues[6] = map[string]any{"id": 1006, "iid": 6, "state": "opened", "title": "Інша", "web_url": "u", "labels": []string{},
		"assignees": []any{}, "references": map[string]string{"full": "grp/app#6"}}
	d.Poll(ctx)
	if err := d.Redo(ctx, "grp/app#6", "x", ""); err == nil {
		t.Fatal("a new task was redone")
	}
}

// A tracker on pause is not searched, and its last failed search no longer says it is down.
func TestPausedTrackerNotSearched(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	tr := d.TrackerList()[0]
	d.mu.Lock()
	d.searches[tr.ID()] = Search{Error: "dial tcp: lookup git.example.com", Down: "GitLab: немає з'єднання"}
	d.mu.Unlock()
	tr.Config().Paused = true
	d.Poll(ctx)
	if _, ok := d.Store.Get("grp/app#5"); ok {
		t.Fatal("a paused tracker was searched")
	}
	if _, ok := d.Searches()[tr.ID()]; ok {
		t.Fatalf("a paused tracker still shows its search: %+v", d.Searches())
	}
	tr.Config().Paused = false
	d.Poll(ctx)
	if _, ok := d.Store.Get("grp/app#5"); !ok {
		t.Fatal("the tracker taken off pause was not searched")
	}
}

// Orca's agent does not report its model: the task keeps the one it was asked for, and a new attempt with
// none is asked for its work's route.
func TestOrcaAgentModel(t *testing.T) {
	d, _, jobs := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Model = "sonnet" })
	d.Take(ctx, "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); len(*jobs) != 1 || (*jobs)[0].Model != "sonnet" || r.AgentModel != "sonnet" {
		t.Fatalf("jobs %+v, agent model %q, error %q", *jobs, r.AgentModel, r.Error)
	}

	d.Store.Update("grp/app#5", func(r *core.Record) { r.Status, r.Model, r.Worktree = core.Queued, "", "" })
	d.Push(ctx, "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); len(*jobs) != 2 || (*jobs)[1].Model != "sonnet" || r.AgentModel != "sonnet" {
		t.Fatalf("jobs %d, agent model %q, error %q", len(*jobs), r.AgentModel, r.Error)
	}
}

// Mako waits to update: nothing starts meanwhile (taken, queued, pushed); once the update is given up, the queue goes on.
func TestHoldWhileUpdateWaits(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	for i := 20; i < 22; i++ {
		f.issues[i] = map[string]any{"id": 1000 + i, "iid": i, "state": "opened", "title": fmt.Sprintf("Task %d", i), "web_url": "u",
			"labels": []string{}, "assignees": []any{}, "references": map[string]string{"full": fmt.Sprintf("grp/app#%d", i)}}
	}
	d.Poll(ctx)
	held := true
	d.Hold = func() string {
		if held {
			return "starts after Mako restarts"
		}
		return ""
	}
	d.Take(ctx, "grp/app#20")
	d.Take(ctx, "grp/app#21")
	d.Wait()
	d.Push(ctx, "grp/app#21")
	d.schedule(ctx)
	d.Wait()
	for _, id := range []string{"grp/app#20", "grp/app#21"} {
		if r, _ := d.Store.Get(id); r.Status != core.Queued {
			t.Fatalf("%s: %s", id, r.Status)
		}
	}
	if len(*jobs) != 0 {
		t.Fatalf("handed %d while held", len(*jobs))
	}
	// The update is cancelled: the queue goes on by itself.
	held = false
	d.schedule(ctx)
	d.Wait()
	if len(*jobs) != 2 {
		t.Fatalf("handed %d after the hold", len(*jobs))
	}
}

// «Перезапустити, коли агенти закінчать хід» holds the queue as well.
func TestHoldWhileRestartPending(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	f.issues[30] = map[string]any{"id": 1030, "iid": 30, "state": "opened", "title": "Task 30", "web_url": "u",
		"labels": []string{}, "assignees": []any{}, "references": map[string]string{"full": "grp/app#30"}}
	d.Poll(ctx)
	d.RequestRestart(true)
	d.Take(ctx, "grp/app#30")
	d.Wait()
	if len(*jobs) != 0 {
		t.Fatal("started while a restart waits")
	}
	d.RequestRestart(false)
	d.schedule(ctx)
	d.Wait()
	if len(*jobs) != 1 {
		t.Fatalf("handed %d", len(*jobs))
	}
}
