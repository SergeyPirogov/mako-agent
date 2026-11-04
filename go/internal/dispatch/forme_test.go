package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// posterTracker: the test's code host that also takes my review.
type posterTracker struct {
	*inboxTracker
	mu       sync.Mutex
	posted   [][]trackers.DraftNote
	heads    []string
	approved []string
	failNote int   // the note (1-based) the host refuses; 0 none
	failAll  error // the review as a whole fails
}

func (p *posterTracker) PostReview(_ context.Context, _ string, head string, notes []trackers.DraftNote) ([]trackers.PostedNote, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failAll != nil {
		return nil, p.failAll
	}
	p.posted, p.heads = append(p.posted, notes), append(p.heads, head)
	out := make([]trackers.PostedNote, len(notes))
	for i := range notes {
		if i+1 == p.failNote {
			out[i].Error = "line refused"
			continue
		}
		out[i].URL = "https://git.example.com/note/" + notes[i].Path
	}
	return out, nil
}

func (p *posterTracker) ApproveReview(_ context.Context, url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failAll != nil {
		return p.failAll
	}
	p.approved = append(p.approved, url)
	return nil
}

const foreignAnswer = "SUMMARY: Додає кеш.\nR1 [must] a.go:1 — кеш не скидається\nR2 [should] b.go — опишіть прапорець\nCHECK:\n- змінити токен\nVERDICT: fix — спершу R1"

// forMeSetup: a dispatcher with one MR to review on a code host that takes reviews, the reviewer faked.
func forMeSetup(t *testing.T, mr core.ReviewRequest) (*Dispatcher, *posterTracker, *[]writer.ForeignMR) {
	d, f, _ := askSetup(t)
	p := &posterTracker{inboxTracker: f}
	d.Trackers = []trackers.Tracker{p}
	f.open = []core.ReviewRequest{mr}
	d.Poll(context.Background())
	if _, ok := d.Reviews.Get(mr.ID); !ok {
		t.Fatal("the MR is not on the board")
	}
	var mu sync.Mutex
	seen := &[]writer.ForeignMR{}
	old := ReviewForMe
	t.Cleanup(func() { ReviewForMe = old })
	ReviewForMe = func(ctx context.Context, dir string, m writer.ForeignMR, route core.Route) (string, error) {
		mu.Lock()
		*seen = append(*seen, m)
		mu.Unlock()
		return foreignAnswer, nil
	}
	return d, p, seen
}

func draftOf(d *Dispatcher, id string) *core.ReviewDraft {
	r, _ := d.Reviews.Get(id)
	return r.Draft
}

func waitDraft(t *testing.T, d *Dispatcher, id string) *core.ReviewDraft {
	t.Helper()
	waitFor(t, func() bool { dr := draftOf(d, id); return dr != nil && dr.State != "running" }, "the reviewer did not finish")
	waitFor(t, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return !d.preparing[id] }, "the run did not end")
	return draftOf(d, id)
}

// «Підготувати ревʼю»: the reviewer's answer is the card's draft, comments to send among it; with no clone of
// the project it reads the diff only; the next search of the code host keeps the draft.
func TestPrepareReview(t *testing.T) {
	mr := mrToReview(1, "aaa")
	d, f, seen := forMeSetup(t, mr)
	if err := d.PrepareReview(mr.ID); err != nil {
		t.Fatal(err)
	}
	dr := waitDraft(t, d, mr.ID)
	if dr.State != "ready" || dr.Summary != "Додає кеш." || dr.Verdict != core.VerdictFix || dr.Commit != "aaa" || !dr.Limited {
		t.Fatalf("draft %+v", dr)
	}
	if len(dr.Comments) != 2 || dr.Comments[0].File != "a.go" || dr.Comments[0].Line != 1 || dr.Comments[0].State != core.DraftOpen {
		t.Fatalf("comments %+v", dr.Comments)
	}
	if len(*seen) != 1 || !(*seen)[0].Limited || !strings.Contains((*seen)[0].Diff, "+++ b/a.go\n@@ -1 +1 @@\n-a\n+b") || (*seen)[0].Ref != "!1" {
		t.Fatalf("reviewer got %+v", *seen)
	}
	// The poll after: the code host knows nothing of the draft, it stays.
	f.open[0].Title = "Зміна 2"
	d.Poll(context.Background())
	if r, _ := d.Reviews.Get(mr.ID); r.Title != "Зміна 2" || r.Draft == nil || len(r.Draft.Comments) != 2 {
		t.Fatalf("after the poll %+v", r)
	}
}

func TestPrepareReviewRefused(t *testing.T) {
	mr := mrToReview(1, "aaa")
	d, _, _ := forMeSetup(t, mr)
	if err := d.PrepareReview("https://nowhere/-/merge_requests/9"); err == nil {
		t.Fatal("prepared an unknown MR")
	}
	// One run at a time.
	release := make(chan struct{})
	ReviewForMe = func(context.Context, string, writer.ForeignMR, core.Route) (string, error) {
		<-release
		return foreignAnswer, nil
	}
	if err := d.PrepareReview(mr.ID); err != nil {
		t.Fatal(err)
	}
	if dr := draftOf(d, mr.ID); dr == nil || dr.State != "running" {
		t.Fatalf("not running: %+v", dr)
	}
	if err := d.PrepareReview(mr.ID); err == nil {
		t.Fatal("two runs at once")
	}
	close(release)
	waitDraft(t, d, mr.ID)
	// A done MR has nothing to review.
	d.finishReview(mr.ID, "merged")
	if err := d.PrepareReview(mr.ID); err == nil {
		t.Fatal("prepared a merged MR")
	}
}

func TestPrepareReviewFails(t *testing.T) {
	mr := mrToReview(1, "aaa")
	d, _, _ := forMeSetup(t, mr)
	ReviewForMe = func(context.Context, string, writer.ForeignMR, core.Route) (string, error) {
		return "", errors.New("claude: exit 1")
	}
	d.PrepareReview(mr.ID)
	if dr := waitDraft(t, d, mr.ID); dr.State != "failed" || dr.Error != "claude: exit 1" {
		t.Fatalf("%+v", dr)
	}
	// A draft that was running when Mako stopped failed.
	d.setDraft(mr.ID, func(dr *core.ReviewDraft) { dr.State = "running" })
	d.interruptedDrafts()
	if dr := draftOf(d, mr.ID); dr.State != "failed" || !strings.Contains(dr.Error, "restarted") {
		t.Fatalf("%+v", dr)
	}
}

// The reviewer's spend goes to the draft and to the statistics' «review for me», with the route of the table's
// review phase for the MR's size.
func TestPrepareReviewSpend(t *testing.T) {
	mr := mrToReview(1, "aaa")
	d, _, _ := forMeSetup(t, mr)
	ReviewForMe = writer.ForeignReview
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$*\" > " + args + "\ncat > /dev/null\n" +
		`printf '%s\n' '{"type":"result","result":"SUMMARY: ok\nVERDICT: merge","total_cost_usd":0.4,"usage":{"input_tokens":300,"output_tokens":50}}'` + "\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	writer.Claude = filepath.Join(dir, "claude")
	d.PrepareReview(mr.ID)
	dr := waitDraft(t, d, mr.ID)
	if dr.State != "ready" || dr.Cost != 0.4 || dr.Tokens != 350 || dr.Verdict != core.VerdictMerge {
		t.Fatalf("%+v", dr)
	}
	sp := d.Spends.All()
	if len(sp) != 1 || sp[0].Phase != core.PhaseForMe || sp[0].USD != 0.4 || sp[0].Project != "grp/lib" {
		t.Fatalf("spends %+v", sp)
	}
	// 4 lines: the table's small review route.
	small := core.DefaultRoutes()[core.PhaseReview]["small"]
	if b, _ := os.ReadFile(args); !strings.Contains(string(b), "--model "+small.Model) || !strings.Contains(string(b), "--effort "+small.Effort) {
		t.Fatalf("args %s", b)
	}
}

func TestForMeRoute(t *testing.T) {
	d, _, _ := askSetup(t)
	mr := mrToReview(1, "aaa")
	for lines, size := range map[int]string{4: "small", 120: "medium", 900: "big"} {
		mr.Additions, mr.Deletions = lines, 0
		r := d.forMeRoute(mr)
		if r.Size != size || r.Model != core.DefaultRoutes()[core.PhaseReview][size].Model || !strings.Contains(r.Why, size) {
			t.Errorf("%d lines: %+v", lines, r)
		}
	}
	// The reviewer's model of the settings weighs more than the table.
	d.Config().Reviewer.Model = "opus"
	if r := d.forMeRoute(mr); r.Model != "opus" {
		t.Errorf("picked: %+v", r)
	}
}

// With the project's clone on this Mac the reviewer reads the MR in a worktree at its head, fetched from the
// code host's ref of the MR; the worktree goes once it is done.
func TestPrepareReviewInClone(t *testing.T) {
	mr := mrToReview(7, "")
	mr.Project = "grp/app" // the clone of setup
	d, _, seen := forMeSetup(t, mr)
	clone, ok := d.forMeClone(mr)
	if !ok {
		t.Fatal("no clone")
	}
	// The code host: a bare repo with the MR's head under refs/merge-requests/7/head.
	remote, work := filepath.Join(t.TempDir(), "remote.git"), t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "mr")
	git(t, work, "init", "-q", "--bare", remote)
	git(t, work, "push", "-q", remote, "HEAD:refs/merge-requests/7/head")
	git(t, clone.Path, "remote", "set-url", "origin", remote)

	var dirSeen string
	ReviewForMe = func(_ context.Context, dir string, m writer.ForeignMR, _ core.Route) (string, error) {
		*seen = append(*seen, m)
		dirSeen = dir
		if _, err := os.Stat(filepath.Join(dir, "a.go")); err != nil && !m.Limited {
			return "", errors.New("the MR's files are not in the worktree")
		}
		return foreignAnswer, nil
	}
	d.PrepareReview(mr.ID)
	if dr := waitDraft(t, d, mr.ID); dr.State != "ready" || dr.Limited {
		t.Fatalf("%+v", dr)
	}
	if (*seen)[0].Limited || filepath.Base(dirSeen) != "mako-review-7" {
		t.Fatalf("reviewed in %q: %+v", dirSeen, (*seen)[0])
	}
	if exists(dirSeen) {
		t.Fatal("the MR's worktree is left behind")
	}
	// The head is gone from the code host: the diff only.
	git(t, work, "push", "-q", remote, ":refs/merge-requests/7/head")
	d.PrepareReview(mr.ID)
	if dr := waitDraft(t, d, mr.ID); dr.State != "ready" || !dr.Limited {
		t.Fatalf("%+v", dr)
	}
}

// The comments I edit, strike out and send: only the chosen ones that are not struck out go, each says how.
func TestSendDrafts(t *testing.T) {
	mr := mrToReview(1, "aaa")
	d, p, _ := forMeSetup(t, mr)
	ctx := context.Background()
	if _, err := d.SendDrafts(ctx, mr.ID, []string{"R1"}); err == nil {
		t.Fatal("sent with no draft")
	}
	d.PrepareReview(mr.ID)
	waitDraft(t, d, mr.ID)

	if err := d.EditDraft(mr.ID, "R1", "  кеш не скидається, коли змінюється токен  ", false); err != nil {
		t.Fatal(err)
	}
	if err := d.EditDraft(mr.ID, "R2", "опишіть прапорець", true); err != nil {
		t.Fatal(err)
	}
	if err := d.EditDraft(mr.ID, "R1", " ", false); err == nil {
		t.Fatal("an empty comment")
	}
	if err := d.EditDraft(mr.ID, "R9", "x", false); err == nil {
		t.Fatal("an unknown comment")
	}
	if _, err := d.SendDrafts(ctx, mr.ID, []string{"R2"}); err == nil {
		t.Fatal("sent a struck-out comment")
	}
	r, err := d.SendDrafts(ctx, mr.ID, []string{"R1", "R2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.posted) != 1 || len(p.posted[0]) != 1 || p.posted[0][0] != (trackers.DraftNote{Path: "a.go", Line: 1, Body: "кеш не скидається, коли змінюється токен"}) || p.heads[0] != "aaa" {
		t.Fatalf("posted %+v %v", p.posted, p.heads)
	}
	if c := r.Draft.Comments; c[0].State != core.DraftSent || c[0].URL == "" || c[1].State != core.DraftDropped {
		t.Fatalf("comments %+v", c)
	}
	// A sent comment is not sent again, nor edited.
	if _, err := d.SendDrafts(ctx, mr.ID, []string{"R1"}); err == nil {
		t.Fatal("sent twice")
	}
	if err := d.EditDraft(mr.ID, "R1", "x", false); err == nil {
		t.Fatal("edited a sent comment")
	}

	// A note the host refuses: failed, with why; sent again later.
	d.EditDraft(mr.ID, "R2", "опишіть прапорець", false)
	p.failNote = 1
	r, _ = d.SendDrafts(ctx, mr.ID, []string{"R2"})
	if c := r.Draft.Comments[1]; c.State != core.DraftFailed || c.Error != "line refused" {
		t.Fatalf("%+v", c)
	}
	p.failNote = 0
	if r, _ = d.SendDrafts(ctx, mr.ID, []string{"R2"}); r.Draft.Comments[1].State != core.DraftSent || r.Draft.Comments[1].Error != "" {
		t.Fatalf("%+v", r.Draft.Comments[1])
	}

	// The review fails as a whole: the error, the comments as they were.
	d.PrepareReview(mr.ID)
	waitDraft(t, d, mr.ID)
	p.failAll = errors.New("403 Forbidden")
	if _, err := d.SendDrafts(ctx, mr.ID, []string{"R1"}); err == nil || err.Error() != "403 Forbidden" {
		t.Fatalf("%v", err)
	}
	if c := draftOf(d, mr.ID).Comments[0]; c.State != core.DraftOpen {
		t.Fatalf("%+v", c)
	}
}

// «Схвалити»: approved from me, the MR goes to «Готово»; a failure leaves it waiting.
func TestApproveReview(t *testing.T) {
	mr := mrToReview(1, "aaa")
	d, p, _ := forMeSetup(t, mr)
	ctx := context.Background()
	p.failAll = errors.New("401")
	if err := d.ApproveReview(ctx, mr.ID); err == nil {
		t.Fatal("approved on a failure")
	}
	if r, _ := d.Reviews.Get(mr.ID); r.State != "open" {
		t.Fatalf("%+v", r)
	}
	p.failAll = nil
	if err := d.ApproveReview(ctx, mr.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Reviews.Get(mr.ID); r.State != "done" || r.Done != "approved" || len(p.approved) != 1 {
		t.Fatalf("%+v %v", r, p.approved)
	}
	if err := d.ApproveReview(ctx, mr.ID); err == nil {
		t.Fatal("approved twice")
	}
}

// A code host that cannot take a review says so.
func TestSendDraftsNoPoster(t *testing.T) {
	d, f, _ := askSetup(t)
	mr := mrToReview(1, "aaa")
	f.open = []core.ReviewRequest{mr}
	d.Poll(context.Background())
	if err := d.ApproveReview(context.Background(), mr.ID); err == nil || !strings.Contains(err.Error(), "cannot send") {
		t.Fatalf("%v", err)
	}
}

func TestFilesDiff(t *testing.T) {
	got := filesDiff([]trackers.ReviewFile{
		{Path: "new.go", Status: "A", Diff: "@@ -0,0 +1 @@\n+x\n"},
		{Path: "b.go", OldPath: "a.go", Status: "R"},
		{Path: "gone.go", Status: "D", Diff: "@@ -1 +0,0 @@\n-y"},
	})
	want := "diff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+x\n" +
		"diff --git a/a.go b/b.go\n--- a/a.go\n+++ b/b.go\n(the diff is not shown: the file is too large or binary)\n" +
		"diff --git a/gone.go b/gone.go\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-y\n"
	if got != want {
		t.Fatalf("%q", got)
	}
}
