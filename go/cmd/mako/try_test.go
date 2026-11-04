package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A copy to try takes the board's looks and the changes that stay in its own data; an action that would start an
// agent, merge or reach a tracker is refused.
func TestTryOnly(t *testing.T) {
	h := tryOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/api/state", true},
		{"HEAD", "/", true},
		{"POST", "/api/settings", true},
		{"POST", "/api/selfstep", true},
		{"POST", "/api/selfskip", true},
		{"POST", "/api/merge", false},
		{"POST", "/api/take", false},
		{"POST", "/api/say", false},
		{"POST", "/api/archive", false},
		{"POST", "/api/trybuild", false},
		{"POST", "/api/settings/worktrees/remove", false},
		{"DELETE", "/api/settings", false},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path+"?id=a%231", nil))
		if got := w.Code == http.StatusTeapot; got != c.ok {
			t.Errorf("%s %s: passed %v, want %v (%d %s)", c.method, c.path, got, c.ok, w.Code, w.Body)
		}
	}
}

// A restart replaces the process (exec): the copies to try are stopped before it, not by a defer that never runs.
func TestRelaunchStopsTries(t *testing.T) {
	var order []string
	d := stopper(func() { order = append(order, "stop") })
	err := relaunchAfterTries(d, func() error { order = append(order, "exec"); return nil })
	if err != nil || len(order) != 2 || order[0] != "stop" || order[1] != "exec" {
		t.Fatalf("order %v, err %v", order, err)
	}
	// A relaunch that fails still leaves the copies stopped: Mako goes on, or quits, without them.
	order = nil
	if err := relaunchAfterTries(d, func() error { return http.ErrAbortHandler }); err != http.ErrAbortHandler || len(order) != 1 {
		t.Fatalf("failed relaunch: %v %v", order, err)
	}
}

type stopper func()

func (f stopper) StopTries() { f() }
