package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// specRun is one run of the agent on an idea, as the stand-in saw it.
type specRun struct {
	dir, base string
	in        writer.SpecInput
}

// specAgent makes the agent answer with the drafts (or errs) in turn, the last one again after them; it keeps the
// runs it was asked for.
func specAgent(t *testing.T, drafts []core.SpecDraft, errs []error) *[]specRun {
	t.Helper()
	var mu sync.Mutex
	runs := &[]specRun{}
	old := SpecFrom
	SpecFrom = func(_ context.Context, dir, base string, _ core.Route, in writer.SpecInput) (core.SpecDraft, error) {
		mu.Lock()
		defer mu.Unlock()
		n := len(*runs)
		*runs = append(*runs, specRun{dir, base, in})
		var err error
		if n < len(errs) {
			err = errs[n]
		}
		return drafts[min(n, len(drafts)-1)], err
	}
	t.Cleanup(func() { SpecFrom = old })
	return runs
}

var specTasks = []core.SplitItem{
	{Title: "Модель експорту", Description: "Типи"},
	{Title: "API експорту", Description: "Хендлер", After: []int{1}},
	{Title: "Кнопка на дошці", Description: "Кнопка", After: []int{2}},
}

func onlySpec(t *testing.T, d *Dispatcher) SpecView {
	t.Helper()
	specs := d.Specs()
	if len(specs) != 1 {
		t.Fatalf("ideas: %+v", specs)
	}
	return specs[0]
}

// An idea goes all the way: the agent asks, the answers come from the board, it proposes the tasks; nothing is in
// the tracker until «Create»; then a parent issue with the specification and its picture, its subtasks in order,
// each waiting for the one before it to be merged.
func TestSpecFromIdeaToTasks(t *testing.T) {
	d, f, _ := setup(t)
	t.Setenv("GIT_SSH_COMMAND", "false")
	ctx := context.Background()
	tracker := d.TrackerList()[0].ID()
	runs := specAgent(t, []core.SpecDraft{
		{Title: "Експорт", Body: "### Мета\nCSV", Questions: []core.SpecQuestion{{Q: "Для кого?", Assume: "для адмінів"}, {Q: "Формат?", Assume: "CSV"}}},
		{Title: "Експорт дошки", Body: "### Мета\nCSV для всіх", Items: specTasks},
	}, nil)

	pic, _, err := d.Attach(NewSpecID, "screen.png", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.NewSpec(tracker, "grp/app", "  Експорт дошки в CSV  ", []string{pic})
	if err != nil {
		t.Fatal(err)
	}
	d.WaitSpecs()
	s := onlySpec(t, d)
	if s.State != "questions" || len(s.Questions) != 2 || s.Text != "Експорт дошки в CSV" || s.Title != "Експорт" {
		t.Fatalf("after the first run: %+v", s)
	}
	if len(s.Images) != 1 || filepath.Dir(s.Images[0]) != d.ScreenshotDir(id) || exists(pic) || len(s.Links) != 1 {
		t.Fatalf("the picture did not move to the idea: %+v", s)
	}
	if r := (*runs)[0]; r.base != "main" || !exists(filepath.Join(r.dir, ".git")) || r.in.Final || r.in.Images[0] != s.Images[0] {
		t.Fatalf("the first run: %+v", r)
	}
	if err := d.CreateSpec(ctx, id, s.Title, s.Body, specTasks); err == nil {
		t.Fatal("tasks created while the agent waits for answers")
	}

	if err := d.AnswerSpec(id, []Answer{{Q: "Для кого?", A: " для всіх "}}); err != nil {
		t.Fatal(err)
	}
	d.WaitSpecs()
	s = onlySpec(t, d)
	if s.State != "ready" || len(s.Items) != 3 || !s.Answered || s.Body != "### Мета\nCSV для всіх" {
		t.Fatalf("after the answers: %+v", s)
	}
	if r := (*runs)[1]; !r.in.Final || r.in.Questions[0].A != "для всіх" || r.in.Questions[1].A != "" {
		t.Fatalf("the second run: %+v", r.in)
	}
	if err := d.AnswerSpec(id, nil); err == nil {
		t.Error("answered twice")
	}
	f.mu.Lock()
	nothing := len(f.created) == 0 && len(f.uploads) == 0 && len(f.notes) == 0
	f.mu.Unlock()
	if !nothing {
		t.Fatalf("the tracker was touched before «Create»: %v %v %v", f.created, f.uploads, f.notes)
	}

	// Edited on the board: the title, the specification, the third task waits for the first as well.
	edited := []core.SplitItem{specTasks[0], specTasks[1], {Title: "Кнопка", Description: "Кнопка", After: []int{1, 2}}}
	if err := d.CreateSpec(ctx, id, "Експорт дошки в CSV", "### Мета\nCSV для всіх, з кнопкою", edited); err != nil {
		t.Fatal(err)
	}
	s = onlySpec(t, d)
	if s.State != "created" || s.Parent != "grp/app#101" || s.Items[2].Task != "grp/app#104" || !strings.HasSuffix(s.URL, "/issues/101") {
		t.Fatalf("created: %+v", s)
	}
	parent := mustGet(t, d, "grp/app#101")
	if parent.Status != core.Split || parent.Breakdown.State != "created" || len(parent.Breakdown.Items) != 3 {
		t.Fatalf("parent %s %+v", parent.Status, parent.Breakdown)
	}
	if desc := parent.Task.Description; !strings.Contains(desc, "з кнопкою") || !strings.Contains(desc, "![pic](/uploads/1/pic.png)") || parent.Task.Title != "Експорт дошки в CSV" {
		t.Fatalf("parent issue %q: %q", parent.Task.Title, desc)
	}
	// The subtasks are #102, #103 and #104: the third waits for the first two.
	third := mustGet(t, d, "grp/app#104")
	if third.ParentID != "grp/app#101" || strings.Join(third.After, ",") != "grp/app#102,grp/app#103" {
		t.Fatalf("the third: parent %q, waits for %v", third.ParentID, third.After)
	}
	if !strings.Contains(third.Task.Description, "Частина #101") || !strings.Contains(third.Task.Description, "Після #102\nПісля #103") {
		t.Fatalf("the third's description %q", third.Task.Description)
	}
	f.mu.Lock()
	commented := strings.Join(f.notes, "\n")
	f.mu.Unlock()
	if !strings.Contains(commented, "#102 Модель експорту") || !strings.Contains(commented, "#104 Кнопка") {
		t.Errorf("the parent's comment: %q", commented)
	}

	// The dependent task waits for the merge of the one before it, even taken.
	second := mustGet(t, d, "grp/app#103")
	if w := d.WaitsFor(second); w == nil || w.On != "grp/app#102" {
		t.Fatalf("the second waits for %+v", w)
	}
	d.Store.Update("grp/app#102", func(r *core.Record) { r.Status, r.ReviewState = core.Done, "merged" })
	if w := d.WaitsFor(mustGet(t, d, "grp/app#103")); w != nil {
		t.Fatalf("the first is merged, yet the second waits for %+v", w)
	}

	// The tracker's search finds the parent: it stays split, nobody takes it.
	d.Poll(ctx)
	if r := mustGet(t, d, "grp/app#101"); r.Status != core.Split {
		t.Fatalf("the parent after a search: %s", r.Status)
	}
	if err := d.DropSpec(id); err != nil || len(d.Specs()) != 0 || exists(d.ScreenshotDir(id)) {
		t.Fatalf("a created idea is not cleared off the board: %v", err)
	}
}

// keepSpec puts an idea into specs.json as it is.
func keepSpec(t *testing.T, d *Dispatcher, s core.Spec) {
	t.Helper()
	d.specsMu.Lock()
	defer d.specsMu.Unlock()
	if err := d.writeSpecs(append(d.readSpecs(), s)); err != nil {
		t.Fatal(err)
	}
}

func readySpec(id, tracker string, items []core.SplitItem) core.Spec {
	return core.Spec{ID: id, Tracker: tracker, Project: "grp/app", Text: "ідея", At: now(), State: "ready", Title: "Ідея", Body: "### Мета\nX", Items: items}
}

// The tracker refuses the second subtask: the parent and the first stay, the idea cannot be dropped, and «Create»
// again opens only the rest.
func TestCreateSpecHalfWay(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	tracker := d.TrackerList()[0].ID()
	keepSpec(t, d, readySpec("spec-1", tracker, specTasks))
	f.mu.Lock()
	f.createLimited, f.createsLeft = true, 2
	f.mu.Unlock()
	err := d.CreateSpec(ctx, "spec-1", "Ідея", "### Мета\nX", specTasks)
	if err == nil || !strings.Contains(err.Error(), "created 1 of 3") {
		t.Fatalf("error %v", err)
	}
	s := onlySpec(t, d)
	if s.State != "ready" || s.Error == "" || s.Parent != "grp/app#101" || s.Items[0].Task != "grp/app#102" || s.Items[1].Task != "" {
		t.Fatalf("after the failure: %+v", s)
	}
	if r := mustGet(t, d, "grp/app#101"); r.Status != core.Split || r.Breakdown.Items[0].Task != "grp/app#102" {
		t.Fatalf("the parent after the failure: %s %+v", r.Status, r.Breakdown)
	}
	if err := d.DropSpec("spec-1"); err == nil {
		t.Error("an idea with tasks in the tracker was dropped")
	}
	if err := d.CreateSpec(ctx, "spec-1", "Ідея", "### Мета\nX", s.Items[:1]); err == nil {
		t.Error("the parent is open, yet one task was taken")
	}

	f.mu.Lock()
	f.createLimited = false
	f.mu.Unlock()
	if err := d.CreateSpec(ctx, "spec-1", "Ідея", "### Мета\nX", s.Items); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	issues := len(f.issues)
	f.mu.Unlock()
	if issues != 5 { // #5, the parent and three subtasks
		t.Fatalf("issues: %d", issues)
	}
	if s := onlySpec(t, d); s.State != "created" || s.Parent != "grp/app#101" {
		t.Fatalf("%+v", s)
	}
}

// An idea of one task is that task, with the specification in its description, and no parent issue.
func TestCreateSpecOneTask(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	keepSpec(t, d, readySpec("spec-1", d.TrackerList()[0].ID(), specTasks[:1]))
	if err := d.CreateSpec(ctx, "spec-1", "Ідея", "### Мета\nX", []core.SplitItem{{Title: "Зробити все", Description: "Опис"}}); err != nil {
		t.Fatal(err)
	}
	s := onlySpec(t, d)
	if s.State != "created" || s.Parent != "" || s.Items[0].Task != "grp/app#101" || s.URL == "" {
		t.Fatalf("%+v", s)
	}
	r := mustGet(t, d, "grp/app#101")
	if !isNew(r) || r.ParentID != "" || r.Task.Title != "Зробити все" || !strings.Contains(r.Task.Description, "### Мета\nX") || !strings.Contains(r.Task.Description, "Опис") {
		t.Fatalf("the task: %s %+v", r.Status, r.Task)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) != 1 {
		t.Fatalf("created %d issues", len(f.created))
	}
}

// What an idea is refused with, and its agent failing, tried again, or cut by Mako stopping.
func TestSpecRefusals(t *testing.T) {
	d, _, _ := setup(t)
	t.Setenv("GIT_SSH_COMMAND", "false")
	ctx := context.Background()
	tracker := d.TrackerList()[0].ID()
	outside := filepath.Join(t.TempDir(), "x.png")
	os.WriteFile(outside, []byte("png"), 0o600)
	for _, c := range []struct {
		tracker, project, text string
		images                 []string
	}{
		{tracker, "grp/app", "  ", nil},
		{tracker, "", "ідея", nil},
		{tracker, "grp/app", strings.Repeat("я", specTextMax+1), nil},
		{"nope", "grp/app", "ідея", nil},
		{tracker, "grp/app", "ідея", []string{outside}},
		{tracker, "grp/app", "ідея", []string{filepath.Join(d.ScreenshotDir(NewSpecID), "gone.png")}},
	} {
		if id, err := d.NewSpec(c.tracker, c.project, c.text, c.images); err == nil {
			t.Errorf("%+v: made %s", c, id)
		}
	}
	if len(d.Specs()) != 0 {
		t.Fatal("a refused idea was kept")
	}

	runs := specAgent(t, []core.SpecDraft{{Title: "Т", Body: "Б", Items: specTasks[:2]}}, []error{errors.New("limit")})
	id, err := d.NewSpec(tracker, "grp/app", "ідея", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.WaitSpecs()
	if s := onlySpec(t, d); s.State != "failed" || s.Error != "limit" {
		t.Fatalf("%+v", s)
	}
	for name, err := range map[string]error{
		"answer": d.AnswerSpec(id, nil),
		"create": d.CreateSpec(ctx, id, "Т", "Б", specTasks[:2]),
		"retry?": d.RetrySpec("spec-404"),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := d.RetrySpec(id); err != nil {
		t.Fatal(err)
	}
	d.WaitSpecs()
	if s := onlySpec(t, d); s.State != "ready" || len(*runs) != 2 {
		t.Fatalf("after «Try again»: %+v, %d runs", s, len(*runs))
	}
	if err := d.RetrySpec(id); err == nil {
		t.Error("a ready idea was run again")
	}
	if err := d.DropSpec(id); err != nil || len(d.Specs()) != 0 {
		t.Fatalf("drop: %v", err)
	}

	// Mako stopped while the agent wrote: the idea is failed, and can be tried again.
	keepSpec(t, d, core.Spec{ID: "spec-old", Tracker: tracker, Project: "grp/app", Text: "ідея", At: now(), State: "running",
		RunAt: time.Now().Add(-specStale - time.Minute).UTC().Format(time.RFC3339Nano)})
	if s := onlySpec(t, d); s.State != "failed" || s.Error == "" {
		t.Fatalf("a stale run: %+v", s)
	}
	if err := d.RetrySpec("spec-old"); err != nil {
		t.Fatal(err)
	}
	d.WaitSpecs()

	// An unreadable specs.json is no ideas, not a failure.
	os.WriteFile(d.specsFile(), []byte("{broken"), 0o600)
	if len(d.Specs()) != 0 {
		t.Fatal("ideas out of a broken file")
	}
}

// GitHub: the parent issue and its subtasks, each assigned to me with the trigger label, the dependent ones saying
// what they wait for.
func TestCreateSpecGitHub(t *testing.T) {
	var mu sync.Mutex
	var created []map[string]any
	var comments []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/repos/me/app/issues" && r.Method == "POST":
			var in map[string]any
			json.Unmarshal(body, &in)
			created = append(created, in)
			n := 10 + len(created)
			out, _ := json.Marshal(map[string]any{"number": n, "state": "open", "title": in["title"], "body": in["body"],
				"html_url": fmt.Sprintf("https://github.com/me/app/issues/%d", n), "labels": []any{map[string]any{"name": "claude-agent"}},
				"assignees": []any{map[string]any{"login": "me"}}})
			w.Write(out)
		case strings.HasPrefix(r.URL.Path, "/repos/me/app/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			var in map[string]any
			json.Unmarshal(body, &in)
			comments = append(comments, fmt.Sprint(in["body"]))
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	tc := &config.Tracker{ID: "gh", Type: "github", APIURL: srv.URL, Trigger: "claude-agent", Projects: map[string]*config.Project{"me/app": {}}}
	gh, err := trackers.NewGitHub(tc, "tok")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, _ := state.Open(dir)
	d := New(&config.Config{StateDir: dir}, store, []trackers.Tracker{gh})
	defer i18n.SetAgentLanguage(i18n.AgentLanguage())
	i18n.SetAgentLanguage(i18n.EN)
	keepSpec(t, d, core.Spec{ID: "spec-1", Tracker: "gh", Project: "me/app", Text: "idea", At: now(), State: "ready", Title: "Export", Body: "### Goal\nCSV", Items: specTasks[:2]})
	if err := d.CreateSpec(context.Background(), "spec-1", "Export", "### Goal\nCSV", []core.SplitItem{{Title: "API"}, {Title: "Button", After: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 3 || created[0]["title"] != "Export" || created[0]["body"] != "### Goal\nCSV" {
		t.Fatalf("created %v", created)
	}
	for _, in := range created {
		if fmt.Sprint(in["assignees"]) != "[me]" || fmt.Sprint(in["labels"]) != "[claude-agent]" {
			t.Errorf("not assigned by the settings: %v", in)
		}
	}
	if b := fmt.Sprint(created[2]["body"]); !strings.Contains(b, "Part of #11") || !strings.Contains(b, "Depends on #12") {
		t.Errorf("the second subtask: %q", b)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "#12 API") || !strings.Contains(comments[0], "#13 Button") {
		t.Errorf("comments %q", comments)
	}
	if r := mustGet(t, d, "me/app#11"); r.Status != core.Split {
		t.Errorf("the parent: %s", r.Status)
	}
	if r := mustGet(t, d, "me/app#13"); strings.Join(r.After, ",") != "me/app#12" || r.ParentID != "me/app#11" {
		t.Errorf("the second subtask waits for %v, of %q", r.After, r.ParentID)
	}
}
