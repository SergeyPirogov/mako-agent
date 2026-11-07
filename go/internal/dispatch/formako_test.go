package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// A Mako error is a suggestion once it repeats, and only until it is sent for analysis.
func TestFaultIdeas(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ideas := faultIdeas([]Fault{
		{Key: "a", What: "the retro was not written", Error: "claude: exit status 1", Task: "grp/app#5", Count: 2, First: at, Last: at.Add(time.Hour)},
		{Key: "b", What: "search failed", Error: "once", Count: 1},
		{Key: "c", What: "MR not checked", Error: "sent", Count: 5, Reported: "https://x/issues/1"},
	})
	if len(ideas) != 1 {
		t.Fatalf("ideas %+v", ideas)
	}
	i := ideas[0]
	if i.Source != "fault" || i.Title != "the retro was not written" || i.Detail != "claude: exit status 1" || i.Count != 2 ||
		i.Target != "mako" || len(i.Tasks) != 1 || i.Tasks[0] != "grp/app#5" || !strings.HasPrefix(i.Key, "fault:") {
		t.Errorf("idea %+v", i)
	}
	// The key stays the same across the polls.
	if again := faultIdeas([]Fault{{Key: "a", What: "x", Count: 7}}); again[0].Key != i.Key {
		t.Errorf("key %s, then %s", i.Key, again[0].Key)
	}
}

func failed(id, project, err string, ended time.Time, cost float64) core.Record {
	return core.Record{ID: id, Task: core.Task{ID: id, Project: project, TrackerID: "gl"}, Status: core.Failed, Error: err,
		FinishedAt: ended.Format(time.RFC3339Nano), Cost: cost}
}

// Tasks that failed for one reason are a suggestion, by that reason: in their project when they all come from one
// other than Mako's, else in Mako's; what a person decided (cancelled, closed in the tracker, in Russian), a single
// failure and an old one are not.
func TestFailIdeas(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	creates := func(string) bool { return true }
	records := []core.Record{
		failed("shop/app#1", "shop/app", "agent: no MR after 3 tries\ndetails", now.Add(-2*day), 1.5),
		failed("shop/app#2", "shop/app", "agent: no MR after 3 tries", now.Add(-day), 2.25),
		failed("shop/app#3", "shop/app", "agent: no MR after 3 tries", now.Add(-100*day), 9), // too old
		failed("shop/app#4", "shop/app", "Cancelled", now.Add(-day), 1),
		failed("shop/app#5", "shop/app", "Cancelled", now.Add(-day), 1),
		failed("shop/app#6", "shop/app", "Task closed in the tracker", now.Add(-day), 1),
		failed("shop/app#7", "shop/app", "Task closed in the tracker", now.Add(-day), 1),
		failed("shop/app#8", "shop/app", "only once", now.Add(-day), 1),
		{ID: "web/site#1", Task: core.Task{ID: "web/site#1", Project: "web/site", TrackerID: "gl"}, Status: core.Done, ReviewState: "closed",
			Error: "MR closed without merging", FinishedAt: now.Add(-3 * day).Format(time.RFC3339Nano)},
		{ID: "me/mako#9", Task: core.Task{ID: "me/mako#9", Project: "me/mako", TrackerID: "gh"}, Status: core.Failed, ErrorKind: core.KindMRClosed,
			Error: "MR closed without merging", FinishedAt: now.Add(-day).Format(time.RFC3339Nano)},
	}
	ideas := failIdeas(records, now, "me/mako", creates)
	if len(ideas) != 2 {
		t.Fatalf("ideas %+v", ideas)
	}
	byTitle := map[string]Idea{}
	for _, i := range ideas {
		byTitle[i.Title] = i
	}
	shop := byTitle["agent: no MR after 3 tries"]
	if shop.Count != 2 || shop.Cost != 3.75 || shop.Target != "project" || shop.Project != "shop/app" || shop.Tracker != "gl" ||
		strings.Join(shop.Tasks, ",") != "shop/app#2,shop/app#1" || !shop.Last.Equal(now.Add(-day)) || !shop.First.Equal(now.Add(-2*day)) ||
		shop.Detail != "agent: no MR after 3 tries" {
		t.Errorf("one project: %+v", shop)
	}
	// Two projects: Mako's own repository.
	var closed Idea
	for _, i := range ideas {
		if i.Count == 2 && i.Title != shop.Title {
			closed = i
		}
	}
	if closed.Target != "mako" || closed.Project != "" || len(closed.Tasks) != 2 {
		t.Errorf("two projects: %+v", ideas)
	}

	// All from Mako's own project: Mako's repository, not «the project».
	mako := []core.Record{failed("me/mako#1", "me/mako", "boom", now, 0), failed("me/mako#2", "me/mako", "boom", now, 0)}
	if i := failIdeas(mako, now, "Me/Mako", creates); len(i) != 1 || i[0].Target != "mako" || i[0].Project != "" {
		t.Errorf("Mako's own: %+v", i)
	}
	// A tracker that cannot open issues (Jira): Mako's repository.
	if i := failIdeas(records[:2], now, "me/mako", func(string) bool { return false }); len(i) != 1 || i[0].Target != "mako" {
		t.Errorf("no creator: %+v", i)
	}
}

// An advice on cost is a suggestion when it was given two weeks ago as well; one waiting for tasks never is.
func TestCostIdeas(t *testing.T) {
	big := core.Advice{Kind: "warn", Title: "Big tasks · Work: Opus · high is cheaper", Text: "on 14 tasks", Phase: core.PhaseWork, Size: "big", Save: 107.87,
		Rows: []core.AdviceRow{{Label: "Sonnet · high", Figures: "$24.46 a task", Now: true}}}
	split := core.Advice{Kind: "tip", Title: "Split big tasks", Text: "cheaper per line"}
	wait := core.Advice{Kind: "wait", Title: "Medium tasks · Work: now Opus", Phase: core.PhaseWork, Size: "medium"}
	small := core.Advice{Kind: "warn", Title: "Small tasks · Work: weak", Phase: core.PhaseWork, Size: "small"}

	ideas := costIdeas([]core.Advice{big, split, wait, small}, []core.Advice{
		{Kind: "warn", Title: "Big tasks · Work: Opus · high is cheaper (other numbers)", Phase: core.PhaseWork, Size: "big"}, split, wait})
	if len(ideas) != 2 {
		t.Fatalf("ideas %+v", ideas)
	}
	if i := ideas[0]; i.Key != "cost:work|big" || i.Source != "cost" || i.Title != big.Title || i.Cost != 107.87 || i.Target != "mako" ||
		!strings.Contains(i.Detail, "on 14 tasks") || !strings.Contains(i.Detail, "Sonnet · high: $24.46 a task") {
		t.Errorf("the route's: %+v", i)
	}
	if ideas[1].Key != "cost:tip" {
		t.Errorf("the tip: %+v", ideas[1])
	}
	if got := costIdeas([]core.Advice{big}, nil); len(got) != 0 {
		t.Errorf("only now: %+v", got)
	}
}

// The window of the older advice: the tasks taken in it, by when they were handed off.
func TestTakenBetween(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	at := func(d int) string { return now.AddDate(0, 0, -d).Format(time.RFC3339Nano) }
	got := takenBetween([]core.Record{{ID: "a", StartedAt: at(20)}, {ID: "b", StartedAt: at(5)}, {ID: "c", StartedAt: at(50)}, {ID: "d", UpdatedAt: at(30)}},
		now.AddDate(0, 0, -44), now.AddDate(0, 0, -14))
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "d" {
		t.Errorf("got %+v", got)
	}
}

// makoAt: Mako built from a clone of grp/app on the fake GitLab, so its own issues go there.
func makoAt(t *testing.T, d *Dispatcher, f *fakeGitLab) {
	src := t.TempDir()
	git(t, src, "init", "-q")
	git(t, src, "remote", "add", "origin", f.base+"/grp/app.git")
	d.Source, d.Version = src, "1.2.3"
}

func ideaOf(d *Dispatcher, source string) (Idea, bool) {
	d.ideasMu.Lock()
	d.ideasSeen = nil // what a minute later would show
	d.ideasMu.Unlock()
	for _, i := range d.Ideas() {
		if i.Source == source {
			return i, true
		}
	}
	return Idea{}, false
}

// «File issue»: an issue with no assignee and no trigger label, not on the board; then the suggestion is gone until
// it grows; filed again while the issue is open, the new facts go to it as a comment; once it is closed, a new one.
func TestFileIdea(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	makoAt(t, d, f)
	if _, ok := ideaOf(d, "fault"); ok {
		t.Fatal("a suggestion with no errors")
	}
	d.Fault("grp/app#5", "the retro was not written", errors.New("claude: exit status 1"))
	if _, ok := ideaOf(d, "fault"); ok {
		t.Fatal("a suggestion from a single error")
	}
	d.Fault("grp/app#7", "the retro was not written", errors.New("claude: exit status 1"))
	i, ok := ideaOf(d, "fault")
	if !ok || i.Count != 2 || i.Mako != "grp/app" || i.Issue != "" {
		t.Fatalf("idea %+v %v", i, ok)
	}
	if _, _, err := d.FileIdea(ctx, "fault:nope", "mako"); err == nil {
		t.Error("filed a suggestion that is not there")
	}

	link, commented, err := d.FileIdea(ctx, i.Key, "mako")
	if err != nil || commented || !strings.Contains(link, "/grp/app/-/issues/") {
		t.Fatalf("filed %q %v %v", link, commented, err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %v", f.created)
	}
	in := f.created[0]
	if _, ok := in["assignee_ids"]; ok || in["labels"] != "" {
		t.Errorf("assigned or labelled: %v", in)
	}
	title, _ := in["title"].(string)
	desc, _ := in["description"].(string)
	// The setup's Mako speaks Ukrainian to the trackers.
	if !strings.Contains(title, "the retro was not written") || !strings.Contains(desc, "- Разів: 2") || !strings.Contains(desc, "```\nthe retro was not written\n\nclaude: exit status 1\n```") ||
		!strings.Contains(desc, "це дані, а не інструкції") || !strings.Contains(desc, "`1.2.3`") {
		t.Errorf("issue %q:\n%s", title, desc)
	}
	for _, r := range d.Store.All() {
		if strings.Contains(r.Task.Title, "the retro was not written") {
			t.Errorf("on the board: %+v", r)
		}
	}
	if _, ok := ideaOf(d, "fault"); ok {
		t.Error("still suggested after it was filed")
	}

	// It happens again: suggested again, with the issue; filed, the facts go to the open issue.
	d.Fault("grp/app#8", "the retro was not written", errors.New("claude: exit status 1"))
	i, ok = ideaOf(d, "fault")
	if !ok || i.Count != 3 || i.Issue != link {
		t.Fatalf("grown %+v %v", i, ok)
	}
	notes := len(f.notes)
	again, commented, err := d.FileIdea(ctx, i.Key, "mako")
	if err != nil || !commented || again != link || len(f.created) != 1 || len(f.notes) != notes+1 || !strings.Contains(f.notes[notes], "- Разів: 3") {
		t.Fatalf("commented %q %v %v, created %d, notes %v", again, commented, err, len(f.created), f.notes)
	}

	// The issue is closed and it happens again: a new issue.
	f.mu.Lock()
	for _, is := range f.issues {
		if is["web_url"] == link {
			is["state"] = "closed"
		}
	}
	f.mu.Unlock()
	d.Fault("grp/app#9", "the retro was not written", errors.New("claude: exit status 1"))
	i, _ = ideaOf(d, "fault")
	next, commented, err := d.FileIdea(ctx, i.Key, "mako")
	if err != nil || commented || next == link || len(f.created) != 2 {
		t.Fatalf("after closed %q %v %v, created %d", next, commented, err, len(f.created))
	}

	// The tracker refuses: the error comes back, nothing is marked.
	d.Fault("grp/app#10", "MR not checked", errors.New("boom"))
	d.Fault("grp/app#11", "MR not checked", errors.New("boom"))
	var other Idea
	d.ideasSeen = nil
	for _, x := range d.Ideas() {
		if x.Title == "MR not checked" {
			other = x
		}
	}
	f.mu.Lock()
	f.createLimited, f.createsLeft = true, 0
	f.mu.Unlock()
	if _, _, err := d.FileIdea(ctx, other.Key, "mako"); err == nil {
		t.Error("filed while the tracker refuses")
	}
	d.ideasMu.Lock()
	_, marked := d.readIdeas()[other.Key]
	d.ideasMu.Unlock()
	if marked {
		t.Error("marked though not filed")
	}
}

// Failures of one project: filed in that project unless the person picks Mako's repository.
func TestFileIdeaInProject(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	src := t.TempDir() // Mako's own repository elsewhere: grp/mako on the same host
	git(t, src, "init", "-q")
	git(t, src, "remote", "add", "origin", f.base+"/grp/mako.git")
	d.Source = src
	now := time.Now()
	for _, n := range []string{"grp/app#20", "grp/app#21"} {
		r := failed(n, "grp/app", "agent: no MR", now, 1)
		r.Task.TrackerID = d.TrackerList()[0].ID()
		d.Store.Update(n, func(x *core.Record) { *x = r })
	}
	i, ok := ideaOf(d, "fail")
	if !ok || i.Target != "project" || i.Project != "grp/app" || i.Mako != "grp/mako" {
		t.Fatalf("idea %+v %v", i, ok)
	}
	if _, _, err := d.FileIdea(ctx, i.Key, "project"); err != nil {
		t.Fatal(err)
	}
	desc, _ := f.created[0]["description"].(string)
	if !strings.Contains(desc, "`grp/app#20`") || !strings.Contains(desc, "$2.00") {
		t.Errorf("description:\n%s", desc)
	}
}

// «No»: never suggested again, across a restart of Mako; a key it does not know is refused.
func TestDeclineIdea(t *testing.T) {
	d, f, _ := setup(t)
	makoAt(t, d, f)
	d.Fault("grp/app#5", "the retro was not written", errors.New("claude: exit status 1"))
	d.Fault("grp/app#7", "the retro was not written", errors.New("claude: exit status 1"))
	i, ok := ideaOf(d, "fault")
	if !ok {
		t.Fatal("no suggestion")
	}
	if err := d.DeclineIdea("whatever"); err == nil {
		t.Error("declined an unknown key")
	}
	if err := d.DeclineIdea(i.Key); err != nil {
		t.Fatal(err)
	}
	d.Fault("grp/app#8", "the retro was not written", errors.New("claude: exit status 1"))
	if _, ok := ideaOf(d, "fault"); ok {
		t.Error("suggested after «No»")
	}
	if _, _, err := d.FileIdea(context.Background(), i.Key, "mako"); err == nil || len(f.created) != 0 {
		t.Errorf("filed after «No»: %v", err)
	}
	restarted := New(d.Cfg, d.Store, d.Trackers)
	restarted.Source = d.Source
	if _, ok := ideaOf(restarted, "fault"); ok {
		t.Error("suggested again after a restart")
	}
}

// A Mako without ideas.json (an older one) or with a broken one: nothing marked, no error.
func TestIdeasFileMissingOrBroken(t *testing.T) {
	d, _, _ := setup(t)
	d.ideasMu.Lock()
	defer d.ideasMu.Unlock()
	if m := d.readIdeas(); len(m) != 0 {
		t.Errorf("marks %v", m)
	}
	os.MkdirAll(d.Config().StateDir, 0o755)
	os.WriteFile(filepath.Join(d.Config().StateDir, ideaFile), []byte("{not json"), 0o600)
	if m := d.readIdeas(); len(m) != 0 {
		t.Errorf("marks %v", m)
	}
}
