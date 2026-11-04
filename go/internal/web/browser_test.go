package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
	"github.com/SergeyPirogov/mako-agent/go/internal/shot"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// The phone's client in a real browser, through a relay that records all it carries: the page pairs from the QR
// link, the board opens, the terminal comes alive, and an update of sw.js keeps the keys. The tape holds no path
// past the app's shell, no cookie, no body; every WebSocket frame on it is a sealed frame.
func TestPhoneClientThroughAnEvilRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("a browser test")
	}
	chrome, err := shot.FindChrome()
	if err != nil {
		t.Skip(err)
	}
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	sess, err := term.Start(term.Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh", Token: "tok",
		Args: []string{"-c", `echo ready; read x; echo "typed $x"; sleep 30`}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	d.Sessions.Put(sess)
	phonesDir := t.TempDir()
	phones, _ := phone.Open(phonesDir)
	mac := httptest.NewServer((&Server{D: d, Phones: phones}).Handler())
	defer mac.Close()
	target, _ := url.Parse(mac.URL)
	rec := &recorder{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = rec
	evil := httptest.NewServer(proxy)
	defer evil.Close()
	code, secret := phones.NewPairingKey()

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	if goruntime.GOOS == "linux" {
		opts = append(opts, chromedp.NoSandbox) // CI's Ubuntu gives Chrome no sandbox of its own
	}
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	boardUp := `location.pathname === "/" && typeof data !== "undefined" && data !== null && data.e2e === true`

	// Pairing, then the board through the service worker.
	// wait for a condition, through the pages' navigations (which stop an evaluation midway).
	wait := func(cond string) {
		t.Helper()
		for end := time.Now().Add(30 * time.Second); ; {
			if ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](cond)); err == nil && ok {
				return
			}
			if time.Now().After(end) {
				where, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`location.href + " " + document.body.innerText.slice(0, 300)`))
				t.Fatalf("waiting for %s: on %s", cond, where)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err := chromedp.Do(ctx, chromedp.Navigate(evil.URL+"/pair#code="+url.QueryEscape(code)+"&k="+url.QueryEscape(secret))); err != nil {
		t.Fatal(err)
	}
	wait(boardUp + ` || document.getElementById("t")?.textContent === "Could not connect"`)
	pairErr, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.getElementById("t")?.textContent === "Could not connect" ? document.getElementById("m").textContent : ""`))
	if err != nil {
		t.Fatal(err)
	}
	if pairErr != "" {
		t.Fatalf("pairing: %s", pairErr)
	}
	if len(phones.Devices()) != 1 || !phones.Devices()[0].E2E {
		t.Fatalf("devices: %+v", phones.Devices())
	}

	// The terminal through makoSocket, with WebSocket's interface.
	if _, err := chromedp.Run(ctx, chromedp.Evaluate[string](`new Promise((ok, no) => {
		const s = makoSocket("/ws/term?id=a%231");
		let got = "", sent = false;
		s.onmessage = (e) => {
			got += JSON.parse(e.data).d || "";
			if (got.includes("ready") && !sent) { sent = true; s.send(JSON.stringify({ t: "i", d: "hello\r" })); }
			if (got.includes("typed hello")) ok(got);
		};
		s.onclose = () => no("closed: " + got);
		setTimeout(() => no("no answer: " + got), 20000);
	})`, chromedp.EvalAwaitPromise)); err != nil {
		t.Fatalf("terminal: %v", err)
	}

	// A new sw.js (here the relay's own) takes over and finds the keys in IndexedDB: the board opens again through
	// it, and tells that the worker is not the one the Mac sent.
	rec.tamper.Store(true)
	if err := chromedp.Do(ctx,
		chromedp.Evaluate[chromedp.Void](`new Promise(async (ok) => {
			navigator.serviceWorker.addEventListener("controllerchange", () => ok(true));
			(await navigator.serviceWorker.getRegistration()).update();
		})`, chromedp.EvalAwaitPromise),
		chromedp.Reload(),
	); err != nil {
		t.Fatalf("after the update: %v", err)
	}
	wait(boardUp + ` && !!document.getElementById("shell-warn")`)

	// The pairing page now comes from the worker itself, not the relay; and when the Mac no longer knows the phone,
	// the board says so and offers to connect again instead of failing request by request.
	if err := chromedp.Do(ctx, chromedp.Navigate(evil.URL+"/pair")); err != nil {
		t.Fatal(err)
	}
	wait(`document.getElementById("t")?.textContent === "Connect the phone" && !document.getElementById("paste").hidden`)
	phones.Remove(phones.Devices()[0].ID)
	if err := chromedp.Do(ctx, chromedp.Navigate(evil.URL+"/")); err != nil {
		t.Fatal(err)
	}
	wait(`document.querySelector("h1")?.textContent === "Cannot reach Mako" && !!document.querySelector('a[href="/pair"]')`)

	// What the relay saw.
	var kept struct{ Devices []phone.Device }
	data, _ := os.ReadFile(filepath.Join(phonesDir, "phones.json"))
	json.Unmarshal(data, &kept)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	clear := func(line string) bool {
		_, u, _ := strings.Cut(line, " ")
		p, _, _ := strings.Cut(u, "?")
		return p == "/pair" || p == "/sw.js" || p == "/style.css" || p == "/favicon.ico" || p == "/e2e" ||
			p == "/manifest.webmanifest" || strings.HasPrefix(p, "/icons/")
	}
	sawTunnel, pairs := false, 0
	for _, l := range rec.lines {
		if l == "GET /pair" {
			pairs++
		}
		if !clear(l) {
			t.Errorf("the relay saw %s", l)
		}
		sawTunnel = sawTunnel || strings.HasPrefix(l, "GET /e2e")
	}
	if !sawTunnel || pairs != 1 {
		t.Fatalf("the tunnel %v, the pairing page from the relay %d times: %v", sawTunnel, pairs, rec.lines)
	}
	tape := rec.tape.String()
	frames := 0
	for _, ws := range rec.sockets {
		for _, side := range [][]byte{ws.up.Bytes(), ws.down.Bytes()} {
			for _, p := range wsFrames(side) {
				var f relay.Sealed
				if err := json.Unmarshal(p, &f); err != nil || len(f.C) == 0 || f.V != relay.Version {
					t.Errorf("a frame that is not sealed: %.200q", p)
				}
				frames++
				tape += string(p)
			}
		}
	}
	if frames < 10 {
		t.Fatalf("only %d frames", frames)
	}
	secrets := map[string]string{"secret": secret, "cookie": deviceCookie, "terminal": "typed hello", "state path": "/api/state",
		"board": "function connect(v)", "xterm": "/vendor/xterm.js"}
	for _, d := range kept.Devices {
		secrets["token"] = d.Token
	}
	for name, s := range secrets {
		if strings.Contains(tape, s) {
			t.Errorf("the relay saw the %s", name)
		}
	}
}

// A hash that does not decode (#100%) and a blocked localStorage must not stop the board's script: the list still loads.
func TestBoardSurvivesBadHashAndBlockedStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("a browser test")
	}
	chrome, err := shot.FindChrome()
	if err != nil {
		t.Skip(err)
	}
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	for _, c := range []struct{ name, hash, script string }{
		{"malformed hash", "#100%", ""},
		{"blocked storage", "", `Storage.prototype.getItem = function () { throw new DOMException("denied", "SecurityError"); };`},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			if c.script != "" {
				if err := chromedp.Do(ctx, chromedp.Func(func(ctx context.Context, tg *chromedp.Target) error {
					_, err := cdp.Call(ctx, tg, cdppage.AddScriptToEvaluateOnNewDocument, cdppage.AddScriptToEvaluateOnNewDocumentParams{Source: c.script})
					return err
				})); err != nil {
					t.Fatal(err)
				}
			}
			if err := chromedp.Do(ctx, chromedp.Navigate(srv.URL+"/"+c.hash)); err != nil {
				t.Fatal(err)
			}
			for end := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
				if ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`typeof data !== "undefined" && data !== null`)); err == nil && ok {
					return
				}
				if time.Now().After(end) {
					t.Fatal("the board never loaded its data")
				}
			}
		})
	}
}

// Resume on a task whose worktree was deleted, pressed on the real board: the server refuses with {ok:false,error},
// and the board says so in a toast (it once redrew in silence). An answer with ok:true shows none.
func TestBoardToastsAFailedAction(t *testing.T) {
	if testing.Short() {
		t.Skip("a browser test")
	}
	chrome, err := shot.FindChrome()
	if err != nil {
		t.Skip(err)
	}
	store, _ := state.Open(t.TempDir())
	store.Update("grp/app#7", func(r *core.Record) {
		r.Status, r.Executor, r.Worktree = core.HandedOff, "terminal", filepath.Join(t.TempDir(), "deleted")
		r.Task = core.Task{ID: "grp/app#7", Title: "Gone", Project: "grp/app"}
	})
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

	if err := chromedp.Do(ctx, chromedp.Navigate(srv.URL+"/")); err != nil {
		t.Fatal(err)
	}
	toastAfter := func(js string) string {
		t.Helper()
		text, err := chromedp.Run(ctx, chromedp.Evaluate[string](`new Promise((ok, no) => { document.getElementById("toast")?.remove(); (async () => { `+js+` })().then(() => ok(document.getElementById("toast")?.textContent ?? ""), (e) => no(String(e))); })`, chromedp.EvalAwaitPromise))
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	if got := toastAfter(`await post("resume", "grp/app#7")`); !strings.Contains(got, "was deleted") {
		t.Errorf("resume without a worktree: toast %q", got)
	}
	if got := toastAfter(`await post("nope", "grp/app#404")`); !strings.Contains(got, "⚠") {
		t.Errorf("a 404: toast %q", got)
	}
	if got := toastAfter(`await post("later", "grp/app#7")`); got != "" {
		t.Errorf("a successful action: toast %q", got)
	}
}
