package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// The block of the steps: rendered with its ticks, read back from among the rest of the description, put in
// place of the old one or after the text.
func TestSelfListBlock(t *testing.T) {
	block := renderSelfList(threeSteps, []bool{true, false})
	for _, want := range []string{selfListStart, "- [x] `/settings`: Зберегти проєкт → Збережено\n", "- [ ] `/`: Відкрити задачу\n", "- [ ] Змерджити\n", selfListEnd} {
		if !strings.Contains(block, want) {
			t.Errorf("no %q in\n%s", want, block)
		}
	}
	desc := "## Що змінено\n- [x] не наш пункт\n\n" + block + "\n\nCloses grp/app#5"
	ticks, ok := parseSelfList(desc)
	if !ok || len(ticks) != 3 || !ticks[0] || ticks[1] || ticks[2] {
		t.Fatalf("ticks %v %v", ticks, ok)
	}
	if ticks, _ := parseSelfList(strings.Replace(desc, "- [ ] Змерджити", "- [X] Змерджити", 1)); !ticks[2] {
		t.Fatal("[X] is a tick too")
	}
	if _, ok := parseSelfList("## Що змінено\n- [x] щось"); ok {
		t.Fatal("a block where there is none")
	}
	again := withSelfList(desc, renderSelfList(threeSteps, []bool{false, false, true}))
	if strings.Count(again, selfListStart) != 1 || !strings.HasPrefix(again, "## Що змінено\n- [x] не наш пункт") || !strings.HasSuffix(again, "Closes grp/app#5") {
		t.Fatalf("replaced:\n%s", again)
	}
	if ticks, _ := parseSelfList(again); ticks[0] || !ticks[2] {
		t.Fatalf("ticks after replace %v", ticks)
	}
	if got := withSelfList("Опис\n\n", block); got != "Опис\n\n"+block {
		t.Fatalf("appended %q", got)
	}
}

// settleSync waits for the board's background syncs of the task to end.
func settleSync(t *testing.T, d *Dispatcher, id string) {
	t.Helper()
	waitFor(t, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return !d.selfSyncing[id] }, "the sync did not end")
}

func mrDescriptionOf(f *fakeGitLab) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, _ := f.mrs[0]["description"].(string)
	return s
}

// GitLab: the list goes into the MR's description, the board's ticks follow it there, the MR's come to the board.
func TestSelfListSyncGitLab(t *testing.T) {
	d, _, id, _, f := selfSetup(t, threeSteps)
	ctx := context.Background()
	d.SyncSelfLists(ctx)
	desc := mrDescriptionOf(f)
	if !strings.HasPrefix(desc, "## Що змінено\n- оплата") || strings.Count(desc, "- [ ] ") != 3 {
		t.Fatalf("description:\n%s", desc)
	}
	// Ticked on the board: ticked in the MR.
	if err := d.SelfStep(id, 0, true, ""); err != nil {
		t.Fatal(err)
	}
	settleSync(t, d, id)
	host := d.hostOf(t, id)
	if err := d.syncSelfList(ctx, host, id); err != nil {
		t.Fatal(err)
	}
	if ticks, _ := parseSelfList(mrDescriptionOf(f)); !ticks[0] || ticks[1] {
		t.Fatalf("MR ticks %v", ticks)
	}
	// Ticked in the MR: ticked on the board, with a line in the journal; cleared in the MR, cleared on the board.
	f.mu.Lock()
	f.mrs[0]["description"] = strings.Replace(f.mrs[0]["description"].(string), "- [ ] Змерджити", "- [x] Змерджити", 1)
	f.mu.Unlock()
	if err := d.syncSelfList(ctx, host, id); err != nil {
		t.Fatal(err)
	}
	r, _ := d.Store.Get(id)
	if v := d.SelfView(r); !v.Steps[2].Done || !v.Steps[0].Done || v.Steps[1].Done || !journalHas(d, id, "step 3 ticked in the MR") {
		t.Fatalf("board after the MR: %+v", v)
	}
	f.mu.Lock()
	f.mrs[0]["description"] = strings.Replace(f.mrs[0]["description"].(string), "- [x] `/settings`", "- [ ] `/settings`", 1)
	f.mu.Unlock()
	if err := d.syncSelfList(ctx, host, id); err != nil {
		t.Fatal(err)
	}
	if r, _ = d.Store.Get(id); d.SelfView(r).Steps[0].Done {
		t.Fatal("cleared in the MR, kept on the board")
	}
	if ticks, _ := parseSelfList(mrDescriptionOf(f)); ticks[0] || !ticks[2] {
		t.Fatalf("MR ticks %v", ticks)
	}
}

func (d *Dispatcher) mustWorktree(t *testing.T, id string) string {
	t.Helper()
	wt, _, err := d.workOf(id)
	if err != nil {
		t.Fatal(err)
	}
	return wt
}

// hostOf: the task's code host, as the dispatcher finds it.
func (d *Dispatcher) hostOf(t *testing.T, id string) trackers.CodeHost {
	t.Helper()
	r, _ := d.Store.Get(id)
	tr := d.tracker(r.Task.TrackerID)
	repo, err := d.route(tr, r.Task)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := d.host(context.Background(), tr, repo)
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// GitHub: the same list in the PR's body, through its API.
func TestSelfListSyncGitHub(t *testing.T) {
	var mu sync.Mutex
	pr := map[string]any{"number": 7, "state": "open", "title": "Pay", "body": "Опис PR", "html_url": "https://github.com/o/r/pull/7"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/repos/o/r/pulls/7" {
			w.WriteHeader(404)
			return
		}
		if r.Method == "PATCH" {
			b, _ := io.ReadAll(r.Body)
			var in map[string]any
			json.Unmarshal(b, &in)
			pr["title"], pr["body"] = in["title"], in["body"]
		}
		json.NewEncoder(w).Encode(pr)
	}))
	defer srv.Close()
	gh, err := trackers.NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL, AllProjects: true, TriggerOff: true}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	d, _, id, _, _ := selfSetup(t, threeSteps[:2])
	d.Store.Update(id, func(r *core.Record) { r.PRURL = "https://github.com/o/r/pull/7" })
	ctx := context.Background()
	if err := d.syncSelfList(ctx, gh, id); err != nil {
		t.Fatal(err)
	}
	body := func() string { mu.Lock(); defer mu.Unlock(); return pr["body"].(string) }
	if !strings.HasPrefix(body(), "Опис PR\n\n"+selfListStart) || strings.Count(body(), "- [ ] ") != 2 || pr["title"] != "Pay" {
		t.Fatalf("body:\n%s", body())
	}
	mu.Lock()
	pr["body"] = strings.Replace(pr["body"].(string), "- [ ] `/`", "- [x] `/`", 1)
	mu.Unlock()
	if err := d.syncSelfList(ctx, gh, id); err != nil {
		t.Fatal(err)
	}
	r, _ := d.Store.Get(id)
	if v := d.SelfView(r); !v.Steps[1].Done || v.Steps[0].Done {
		t.Fatalf("board after the PR: %+v", v)
	}
	// Ticked on the board (as SelfStep does, without its sync through the repo's GitLab origin): ticked in the PR.
	d.setTick(id, threeSteps[:2], 0, true, headIn(t, d.mustWorktree(t, id)))
	if err := d.syncSelfList(ctx, gh, id); err != nil {
		t.Fatal(err)
	}
	if ticks, _ := parseSelfList(body()); len(ticks) != 2 || !ticks[0] || !ticks[1] {
		t.Fatalf("PR ticks %v in\n%s", ticks, body())
	}
}
