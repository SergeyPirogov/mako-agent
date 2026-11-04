package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// A body over its route's limit is refused with «too long», never cut and passed on as a whole message or rule: with
// max bytes the route goes on (and answers its own way), with max+1 it stops at the limit.
func TestBodyOverLimitIsRefused(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	store.Update("grp/app#7", func(r *core.Record) { r.Task = core.Task{ID: "grp/app#7", Title: "T", Project: "grp/app"} })
	mr := "https://gl/grp/app/-/merge_requests/4"
	d.Reviews.Update(mr, func(r *core.ReviewRequest) { r.TrackerID, r.Project, r.State, r.URL = "gone", "grp/app", "open", mr })

	post := func(path, body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	id := url.QueryEscape("grp/app#7")
	// A review-request body is JSON: padding inside a string keeps it valid at exactly max bytes.
	pad := func(n int) string {
		const head, tail = `{"comment":"R1","text":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	for _, c := range []struct {
		name, path string
		max        int
		body       func(n int) string
	}{
		{"say", "/api/say?id=" + id, 200 << 10, func(n int) string { return strings.Repeat("я", n/2) + strings.Repeat("x", n%2) }},
		{"say in a free chat", "/api/say?id=chat-abc", 200 << 10, func(n int) string { return strings.Repeat("x", n) }},
		{"lesson", "/api/lesson?id=" + id + "&to=base", 8 << 10, func(n int) string { return strings.Repeat("x", n) }},
		{"navretro save", "/api/navretro/save?key=k&id=1", 8 << 10, func(n int) string { return strings.Repeat("x", n) }},
		{"lessons answer", "/api/settings/lessons/answer?id=1&to=base", 4096, func(n int) string { return strings.Repeat("x", n) }},
		{"review-request", "/api/review-request/draft?id=" + url.QueryEscape(mr), 200 << 10, pad},
	} {
		if out := post(c.path, c.body(c.max)); out["error"] == errTooLong {
			t.Errorf("%s: %d bytes refused: %v", c.name, c.max, out)
		}
		out := post(c.path, c.body(c.max+1))
		if out["ok"] != false || !strings.Contains(out["error"].(string), errTooLong) {
			t.Errorf("%s: %d bytes: %v", c.name, c.max+1, out)
		}
	}
	// 3 MB to review-request is read up to the limit only.
	if out := post("/api/review-request/draft?id=nope", pad(3<<20)); out["ok"] != false || !strings.Contains(out["error"].(string), errTooLong) {
		t.Errorf("3 MB: %v", out)
	}
}
