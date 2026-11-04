package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/shot"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// By project, every section of a project is a column and folds by a click on its heading: «Ready to merge», «In progress»
// and «New» start open, «Done» (like «Postponed» and «Not done») folded; the fold is remembered across a reload; while
// filtering, a click folds nothing; a broken or an old localStorage does not break the list.
func TestTreeSectionsFold(t *testing.T) {
	if testing.Short() {
		t.Skip("a browser test")
	}
	chrome, err := shot.FindChrome()
	if err != nil {
		t.Skip(err)
	}
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	if goruntime.GOOS == "linux" {
		opts = append(opts, chromedp.NoSandbox)
	}
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// The board with fixed rows (the page's own polling would replace them, so each script draws and acts at once).
	const setup = `
		const row = (id, column, extra) => ({ id, column, status: "handed-off", task: { id, title: "Task " + id, project: "g/app" }, ...extra });
		data = { ...data, records: [
			row("g/app#1", "review", { mergeReady: true, agentReviews: [{}], prUrl: "https://x/pr/1" }),
			row("g/app#2", "doing"), row("g/app#3", "doing"), row("g/app#4", "new"), row("g/app#5", "done"),
		] };
		groupBy = "project"; project = ""; query = ""; $("q").value = "";
		const sec = (n) => document.querySelector('#list .quiet[data-sub="g/app|' + n + '"]');
		const items = () => document.querySelectorAll("#list .item").length;
	`
	eval := func(js string, out any) {
		t.Helper()
		raw, err := chromedp.Run(ctx, chromedp.Evaluate[string](`(() => {`+setup+js+`})()`))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			t.Fatalf("%v: %q", err, raw)
		}
	}
	boardUp := func(what string) {
		t.Helper()
		for end := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if up, _ := chromedp.Run(ctx, chromedp.Evaluate[bool](`typeof data !== "undefined" && data !== null && typeof render === "function"`)); up {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("the board did not come up %s", what)
			}
		}
	}
	if _, err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/")); err != nil {
		t.Fatal(err)
	}
	boardUp("at the start")

	type shape struct {
		Open   map[string]string `json:"open"`  // section → aria-expanded
		Count  map[string]string `json:"count"` // section → counter
		Items  int               `json:"items"`
		Merge  bool              `json:"merge"`  // the merge heading keeps its class
		Tip    string            `json:"tip"`    // and its hint
		Stored string            `json:"stored"` // localStorage["mako-folded-subs"]
	}
	const read = `
		const o = {}, c = {};
		for (const n of ["merge", "doing", "new", "done"]) { const b = sec(n); o[n] = b ? b.getAttribute("aria-expanded") : "-"; c[n] = b ? b.querySelector(".n").textContent : "-"; }
		return JSON.stringify({ open: o, count: c, items: items(), merge: !!sec("merge")?.classList.contains("merge"), tip: sec("merge")?.title || "", stored: localStorage.getItem("mako-folded-subs") || "" });
	`
	var got shape
	// 1. At the start «Ready to merge», «In progress» and «New» are open, «Done» folded, with counters; «Ready to merge» is green and has its hint.
	eval(`localStorage.removeItem("mako-folded-subs"); foldedSubs = {}; render(); `+read, &got)
	if got.Open["merge"] != "true" || got.Open["doing"] != "true" || got.Open["new"] != "true" || got.Open["done"] != "false" || got.Items != 4 {
		t.Fatalf("at the start: %+v", got)
	}
	if got.Count["merge"] != "1" || got.Count["doing"] != "2" || got.Count["new"] != "1" || got.Count["done"] != "1" || !got.Merge || got.Tip == "" {
		t.Fatalf("counters/merge: %+v", got)
	}
	// 2. A click folds «In progress»: its tasks go, the counter stays, the others stay as they were, the fold is stored.
	eval(`render(); sec("doing").click(); `+read, &got)
	if got.Open["doing"] != "false" || got.Open["merge"] != "true" || got.Open["new"] != "true" || got.Items != 2 || got.Count["doing"] != "2" || got.Stored != `{"g/app|doing":true}` {
		t.Fatalf("after folding: %+v", got)
	}
	// 3. After a reload the fold stays (the state is read from localStorage); a second click opens it again.
	if _, err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	boardUp("after the reload")
	eval(`render(); `+read, &got)
	if got.Open["doing"] != "false" || got.Open["merge"] != "true" || got.Items != 2 {
		t.Fatalf("after the reload: %+v", got)
	}
	eval(`sec("doing").click(); `+read, &got)
	if got.Open["doing"] != "true" || got.Items != 4 || got.Stored != `{"g/app|doing":false}` {
		t.Fatalf("after unfolding: %+v", got)
	}
	// 4. A click opens «Done», which starts folded; after a reload it stays open.
	eval(`sec("done").click(); `+read, &got)
	if got.Open["done"] != "true" || got.Items != 5 {
		t.Fatalf("after opening «Done»: %+v", got)
	}
	if _, err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	boardUp("after the second reload")
	eval(`render(); `+read, &got)
	if got.Open["done"] != "true" || got.Items != 5 {
		t.Fatalf("«Done» after the reload: %+v", got)
	}
	// 5. While filtering, every section is open (also «Done») and a click folds nothing.
	eval(`localStorage.removeItem("mako-folded-subs"); foldedSubs = {"g/app|new": true}; query = "task"; $("q").value = "task"; render(); sec("new").click(); sec("merge").click(); `+read, &got)
	if got.Open["new"] != "true" || got.Open["merge"] != "true" || got.Open["doing"] != "true" || got.Open["done"] != "true" || got.Items != 5 || got.Stored != "" {
		t.Fatalf("while filtering: %+v", got)
	}
	// 6. The old value (a list of the folded sections) still works: those sections are folded, the rest as by default.
	eval(`localStorage.setItem("mako-folded-subs", '["g/app|new"]'); query = ""; $("q").value = "";`+"return JSON.stringify({});", &struct{}{})
	if _, err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	boardUp("with the old localStorage")
	eval(`render(); `+read, &got)
	if got.Open["new"] != "false" || got.Open["doing"] != "true" || got.Open["done"] != "false" {
		t.Fatalf("old store: %+v", got)
	}
	// 7. A broken value in localStorage: the page still starts with the default folds.
	eval(`localStorage.setItem("mako-folded-subs", "{not json");`+"return JSON.stringify({});", &struct{}{})
	if _, err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	boardUp("with a broken localStorage")
	eval(`render(); `+read, &got)
	if got.Open["doing"] != "true" || got.Open["new"] != "true" || got.Open["done"] != "false" {
		t.Fatalf("broken store: %+v", got)
	}
}
