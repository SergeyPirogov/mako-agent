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

// By project, every section of a project folds by a click on its heading: «Ready to merge», «In progress» and «New»
// start open, «Postponed» and «Done» folded; the fold is remembered across a reload; while filtering, a click folds
// nothing; a broken localStorage does not break the list.
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
			row("g/app#2", "doing"), row("g/app#3", "doing"), row("g/app#4", "new"),
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
		for (const n of ["merge", "work", "new"]) { const b = sec(n); o[n] = b ? b.getAttribute("aria-expanded") : "-"; c[n] = b ? b.querySelector(".n").textContent : "-"; }
		return JSON.stringify({ open: o, count: c, items: items(), merge: !!sec("merge")?.classList.contains("merge"), tip: sec("merge")?.title || "", stored: localStorage.getItem("mako-folded-subs") || "" });
	`
	var got shape
	// 1. All three open at the start, with counters, «Ready to merge» green and with its hint.
	eval(`localStorage.removeItem("mako-folded-subs"); foldedSubs = new Set(); render(); `+read, &got)
	if got.Open["merge"] != "true" || got.Open["work"] != "true" || got.Open["new"] != "true" || got.Items != 4 {
		t.Fatalf("at the start: %+v", got)
	}
	if got.Count["merge"] != "1" || got.Count["work"] != "2" || got.Count["new"] != "1" || !got.Merge || got.Tip == "" {
		t.Fatalf("counters/merge: %+v", got)
	}
	// 2. A click folds «In progress»: its tasks go, the counter stays, the others stay open, the fold is stored.
	eval(`render(); sec("work").click(); `+read, &got)
	if got.Open["work"] != "false" || got.Open["merge"] != "true" || got.Open["new"] != "true" || got.Items != 2 || got.Count["work"] != "2" || got.Stored != `["g/app|work"]` {
		t.Fatalf("after folding: %+v", got)
	}
	// 3. After a reload the fold stays (the state is read from localStorage); a second click opens it again.
	if _, err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	boardUp("after the reload")
	eval(`render(); `+read, &got)
	if got.Open["work"] != "false" || got.Open["merge"] != "true" || got.Items != 2 {
		t.Fatalf("after the reload: %+v", got)
	}
	eval(`sec("work").click(); `+read, &got)
	if got.Open["work"] != "true" || got.Items != 4 || got.Stored != `[]` {
		t.Fatalf("after unfolding: %+v", got)
	}
	// 4. While filtering, every section is open and a click folds nothing.
	eval(`foldedSubs = new Set(["g/app|new"]); query = "task"; $("q").value = "task"; render(); sec("new").click(); sec("merge").click(); `+read, &got)
	if got.Open["new"] != "true" || got.Open["merge"] != "true" || got.Open["work"] != "true" || got.Items != 4 || got.Stored != `[]` {
		t.Fatalf("while filtering: %+v", got)
	}
	// 5. A broken value in localStorage: the page still starts with every section open.
	eval(`localStorage.setItem("mako-folded-subs", "{not json"); query = ""; $("q").value = "";`+"return JSON.stringify({});", &struct{}{})
	if _, err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	boardUp("with a broken localStorage")
	eval(`render(); `+read, &got)
	if got.Open["work"] != "true" {
		t.Fatalf("broken store: %+v", got)
	}
}
