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
