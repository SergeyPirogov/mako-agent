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

// Three small things of the board, on the real page: the hint of the «Fix» button is escaped (a branch name may hold
// quotes and brackets); an old answer of /api/state that comes after a newer one does not put the old board back; the
// drafts kept in the browser for tasks and chats that are gone are dropped (and those of the live ones are not).
func TestBoardLoadOrderFixHintAndDrafts(t *testing.T) {
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

	if _, err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/")); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if up, _ := chromedp.Run(ctx, chromedp.Evaluate[bool](`typeof data !== "undefined" && data !== null && typeof load === "function"`)); up {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the board did not come up")
		}
	}
	// js is the body of an async function that returns a value; the value comes back as JSON.
	run := func(js string, out any) {
		t.Helper()
		raw, err := chromedp.Run(ctx, chromedp.Evaluate[string](`(async () => {`+js+`})().then((v) => JSON.stringify(v))`, chromedp.EvalAwaitPromise))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			t.Fatalf("%v: %q", err, raw)
		}
	}

	// 1. A branch name with quotes and brackets stays inside the hint: no element comes out of it, the hint is the text.
	var fix struct {
		Title string `json:"title"`
		Label string `json:"label"`
		Img   bool   `json:"img"`
		Attrs int    `json:"attrs"`
	}
	run(`
		const label = 'The agent will update the branch from x"><img src=y onerror=alert(1)>';
		const box = document.createElement("div");
		box.innerHTML = fixBtn("g/app#1", true, "rebase", label);
		const b = box.querySelector("button");
		return { title: b.title, label, img: !!box.querySelector("img"), attrs: b.attributes.length };`, &fix)
	if fix.Img || fix.Title != fix.Label || fix.Attrs != 5 {
		t.Fatalf("fixBtn does not escape its hint: %+v", fix)
	}

	// 2. Two reloads in flight: the older one answers last and must not replace the newer board. (The page's own
	// polling is let through; only the first two /api/state requests after the stub is set are held.)
	var order struct {
		Column string `json:"column"`
		Error  string `json:"error"`
	}
	run(`
		const real = window.fetch, held = [];
		window.fetch = (url, init) => {
			if (String(url).startsWith("/api/state") && held.length < 2) {
				return new Promise((ok) => held.push((body) => ok({ ok: true, json: async () => body })));
			}
			return real(url, init);
		};
		const board = (column) => ({ ...data, digest: null, records: [{ id: "g/app#1", column, status: "queued", task: { id: "g/app#1", title: "T", project: "g/app" } }] });
		try {
			const old = load(), fresh = load();           // the older request is asked first
			held[1](board("doing")); await fresh;          // the newer answer comes first
			held[0](board("new"));  await old;             // the older one comes after it
			await new Promise((r) => setTimeout(r, 50));
			return { column: data.records[0]?.column, error: $("search").className };
		} finally { window.fetch = real; }`, &order)
	if order.Column != "doing" {
		t.Fatalf("an old answer replaced the newer board: card in %q, want «doing»", order.Column)
	}
	// An old request that fails after the newer one has answered says nothing: the board is fine.
	run(`
		const real = window.fetch, held = [];
		window.fetch = (url, init) => {
			if (String(url).startsWith("/api/state") && held.length < 2) {
				return new Promise((ok, no) => held.push({ ok, no }));
			}
			return real(url, init);
		};
		try {
			$("search").className = "search"; $("search").textContent = "";
			const old = load(), fresh = load();
			held[1].ok({ ok: true, json: async () => ({ ...data, digest: null }) }); await fresh;
			held[0].no(new Error("down")); await old;
			return { error: $("search").className };
		} finally { window.fetch = real; }`, &order)
	if order.Error == "search err" {
		t.Fatalf("an old failed request put the error on a board that answered")
	}

	// 3. Drafts of tasks and chats that are gone are dropped from the memory and from localStorage; the live ones stay;
	// a board with no tasks drops nothing.
	type kept struct {
		Notes, Answers, Tickets []string
	}
	var got struct {
		Before, After, Empty, Memory kept
	}
	run(`
		const keys = (o) => Object.keys(o).sort();
		const row = (id) => ({ id, column: "doing", status: "queued", task: { id, title: "T", project: "g/app" } });
		const stored = (k) => keys(JSON.parse(localStorage.getItem(k) || "{}"));
		const shape = () => ({ Notes: stored("mako-notes"), Answers: stored("mako-answers"), Tickets: stored("mako-tickets") });
		notes = { "g/app#1": [{ id: 1 }], "g/app#9": [{ id: 2 }] }; saveNotes();
		answerDrafts = { "g/app#1": { q: "a" }, "g/app#8": { q: "b" } }; saveAnswers();
		freeChats = [{ id: "chat-live" }];
		localStorage.setItem("mako-tickets", JSON.stringify({
			"chat-live#2": { id: "g/app#5", title: "a" }, "chat-live#3#plan": { id: "g/app#6", title: "b" },
			"chat-gone#1": { id: "g/app#7", title: "c" }, "chat-gone#4#plan": { id: "g/app#7", title: "d" },
			"g/app#1#5": { id: "g/app#10", title: "e" }, "g/app#2#5": { id: "g/app#11", title: "f" } }));
		const before = shape();
		// no tasks at all: nothing is dropped
		const saved = data.records; data.records = []; pruneDrafts(); pruneTickets();
		const empty = shape();
		data.records = [row("g/app#1")];
		pruneDrafts(); pruneTickets();
		const after = shape();
		data.records = saved;
		return { Before: before, After: after, Empty: empty, Memory: { Notes: keys(notes), Answers: keys(answerDrafts), Tickets: [] } };`, &got)
	want := kept{Notes: []string{"g/app#1"}, Answers: []string{"g/app#1"}, Tickets: []string{"chat-live#2", "chat-live#3#plan", "g/app#1#5"}}
	if len(got.Before.Notes) != 2 || len(got.Before.Tickets) != 6 {
		t.Fatalf("the setup: %+v", got.Before)
	}
	if len(got.Empty.Notes) != 2 || len(got.Empty.Answers) != 2 || len(got.Empty.Tickets) != 6 {
		t.Fatalf("a board without tasks dropped drafts: %+v", got.Empty)
	}
	if !same(got.After.Notes, want.Notes) || !same(got.After.Answers, want.Answers) || !same(got.After.Tickets, want.Tickets) {
		t.Fatalf("after the pruning: %+v, want %+v", got.After, want)
	}
	if !same(got.Memory.Notes, want.Notes) || !same(got.Memory.Answers, want.Answers) {
		t.Fatalf("in memory: %+v, want %+v", got.Memory, want)
	}
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
