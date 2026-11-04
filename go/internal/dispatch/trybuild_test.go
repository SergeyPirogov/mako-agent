package dispatch

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// TestTryHelper is not a test: run by the try command of the tests below (GO_TRY_HELPER=1), it is a project's
// build that serves «try» on $PORT until it is stopped.
func TestTryHelper(t *testing.T) {
	if os.Getenv("GO_TRY_HELPER") != "1" {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		os.Exit(2)
	}
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "try "+r.URL.Path) }))
}

// helperTry: the try command that runs TestTryHelper.
func helperTry() string {
	return fmt.Sprintf("GO_TRY_HELPER=1 exec %q -test.run='^TestTryHelper$'", os.Args[0])
}

// setTry gives the task's repo the try command.
func setTry(cfg *config.Config, command string) {
	for _, r := range cfg.Trackers[0].Projects["grp/app"].Repos {
		r.Try = command
	}
}

// opened: the pages «Try this build» opened, instead of the browser.
func opened(t *testing.T) func() []string {
	var mu sync.Mutex
	var pages []string
	old := OpenTry
	OpenTry = func(url string) { mu.Lock(); pages = append(pages, url); mu.Unlock() }
	t.Cleanup(func() { OpenTry = old })
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), pages...) }
}

func tryState(d *Dispatcher, id string) *TryView {
	r, _ := d.Store.Get(id)
	return d.tryView(id, d.selfHead(r))
}

// A project's try command runs in the task's worktree on $PORT; the copy opens on the first step's page, a tick
// needs it (or a reason), «Try this build» again only opens it, Stop and a closed card end it.
func TestTryBuildCommand(t *testing.T) {
	pages := opened(t)
	d, cfg, id, wt, _ := selfSetup(t, threeSteps, func(c *config.Config) { setTry(c, helperTry()) })
	t.Cleanup(d.StopTries)
	r, _ := d.Store.Get(id)
	if v := d.SelfView(r); !v.CanTry || v.Tried || v.Try != nil {
		t.Fatalf("before the try: %+v", v)
	}
	if err := d.SelfStep(id, 0, true, ""); err != errNotTried {
		t.Fatalf("ticked before the try: %v", err)
	}
	if err := d.SelfStep(id, 0, true, "дивився на своїй збірці"); err != nil {
		t.Fatal(err)
	}
	if !journalHas(d, id, "without a run: дивився на своїй збірці") {
		t.Fatalf("journal %+v", d.Logs(id))
	}

	if err := d.TryBuild(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v := tryState(d, id); return v != nil && v.State != "starting" }, "the copy did not come up")
	v := tryState(d, id)
	if v.State != "running" || !v.Fresh || !strings.HasPrefix(v.URL, "http://127.0.0.1:") {
		t.Fatalf("try %+v", v)
	}
	if p := pages(); len(p) != 1 || p[0] != v.URL+"/settings" {
		t.Fatalf("opened %v", p)
	}
	res, err := http.Get(v.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "try /settings" {
		t.Fatalf("the copy answered %q", body)
	}
	r, _ = d.Store.Get(id)
	if r.SelfCheck.Tried == nil || r.SelfCheck.Tried.Commit != headIn(t, wt) || !d.SelfView(r).Tried {
		t.Fatalf("tried %+v", r.SelfCheck.Tried)
	}
	if err := d.SelfStep(id, 1, true, ""); err != nil {
		t.Fatalf("a tick after the try: %v", err)
	}

	// Again at the same HEAD: the same copy, opened again.
	if err := d.TryBuild(id); err != nil {
		t.Fatal(err)
	}
	if p := pages(); len(p) != 2 || tryState(d, id).URL != v.URL {
		t.Fatalf("again: %v, %+v", p, tryState(d, id))
	}

	// Stop ends the command.
	d.TryStop(id)
	if tryState(d, id) != nil {
		t.Fatal("the copy stayed")
	}
	waitFor(t, func() bool { _, err := http.Get(v.URL); return err != nil }, "the copy still answers after Stop")

	// A card no board has had open for a while: its copy is stopped.
	if err := d.TryBuild(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v := tryState(d, id); return v != nil && v.State == "running" }, "the copy did not come up again")
	d.TryKeep(id)
	d.reapTries(time.Now().Add(tryIdle / 2))
	if tryState(d, id) == nil {
		t.Fatal("a kept copy was stopped")
	}
	d.reapTries(time.Now().Add(2 * tryIdle))
	if tryState(d, id) != nil {
		t.Fatal("an idle copy was kept")
	}

	// A command that fails says why.
	setTry(cfg, "echo boom; exit 3")
	if err := d.TryBuild(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v := tryState(d, id); return v != nil && v.State == "failed" }, "the failure was not seen")
	if v := tryState(d, id); !strings.Contains(v.Error, "boom") {
		t.Fatalf("failed: %+v", v)
	}
}

// Mako's own repository tries its build without a command: ServeMako, the board's main page when no step names
// one; a project with neither has no «Try this build».
func TestTryBuildMako(t *testing.T) {
	pages := opened(t)
	d, _, id, wt, _ := selfSetup(t, []core.CheckStep{{Do: "Глянути"}})
	t.Cleanup(d.StopTries)
	r, _ := d.Store.Get(id)
	if d.SelfView(r).CanTry {
		t.Fatal("try without a command or Mako")
	}
	if err := d.TryBuild(id); err == nil {
		t.Fatal("tried without a command or Mako")
	}
	os.MkdirAll(wt+"/go/cmd/mako", 0o755)
	os.WriteFile(wt+"/go/cmd/mako/main.go", []byte("package main"), 0o644)
	old := ServeMako
	t.Cleanup(func() { ServeMako = old })
	stopped := make(chan struct{})
	ServeMako = func(ctx context.Context, worktree, home string) (string, func(), error) {
		if worktree != wt {
			t.Errorf("built %s", worktree)
		}
		return "http://127.0.0.1:1", func() { close(stopped) }, nil
	}
	if r, _ = d.Store.Get(id); !d.SelfView(r).CanTry {
		t.Fatal("no try in Mako's repository")
	}
	if err := d.TryBuild(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v := tryState(d, id); return v != nil && v.State == "running" }, "Mako's copy did not come up")
	if p := pages(); len(p) != 1 || p[0] != "http://127.0.0.1:1/" {
		t.Fatalf("opened %v", p)
	}
	d.StopTries()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Mako's copy not stopped with Mako")
	}
}
