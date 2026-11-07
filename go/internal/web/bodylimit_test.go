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
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
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
		{"redo", "/api/redo?id=" + id, 200 << 10, func(n int) string { return `{"Note":"` + strings.Repeat("x", n-len(`{"Note":""}`)) + `"}` }},
		{"review-request", "/api/review-request/draft?id=" + url.QueryEscape(mr), 200 << 10, pad},
		{"idea", "/api/specs/new", specBodyMax, func(n int) string { return `{"text":"` + strings.Repeat("x", n-len(`{"text":""}`)) + `"}` }},
		{"eval run", "/api/eval/run?key=k", evalBodyMax, func(n int) string {
			return `{"variant":{"claudeMd":"` + strings.Repeat("x", n-len(`{"variant":{"claudeMd":""}}`)) + `"}}`
		}},
	} {
		if out := post(c.path, c.body(c.max)); out["error"] == errTooLong || out["error"] == "bad request: "+errTooLong {
			t.Errorf("%s: %d bytes refused: %v", c.name, c.max, out)
		}
		out := post(c.path, c.body(c.max+1))
		// Exactly «too long» (or the review-request's «bad request: too long»): the answer is the handler's own, not
		// the error of a call that got the cut body.
		if got, _ := out["error"].(string); out["ok"] != false || (got != errTooLong && got != "bad request: "+errTooLong) {
			t.Errorf("%s: %d bytes: %v", c.name, c.max+1, out)
		}
	}
	// 3 MB to review-request is read up to the limit only.
	if out := post("/api/review-request/draft?id=nope", pad(3<<20)); out["ok"] != false || !strings.Contains(out["error"].(string), errTooLong) {
		t.Errorf("3 MB: %v", out)
	}
}

// decodeJSON, shared by many routes: a body of exactly the limit is read, one byte more is refused as «too long»
// (not left to fail as a cut JSON), an empty one leaves the value as it was, a broken one is an error.
func TestDecodeJSONLimit(t *testing.T) {
	type in struct{ A string }
	body := func(n int) string { return `{"A":"` + strings.Repeat("x", n-len(`{"A":""}`)) + `"}` }
	read := func(b string) (in, error) {
		var v in
		err := decodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(b)), 64, &v)
		return v, err
	}
	if v, err := read(body(64)); err != nil || len(v.A) != 64-len(`{"A":""}`) {
		t.Errorf("at the limit: %v %+v", err, v)
	}
	if v, err := read(body(65)); err == nil || err.Error() != errTooLong || v.A != "" {
		t.Errorf("over the limit: %v %+v", err, v)
	}
	if v, err := read(""); err != nil || v.A != "" {
		t.Errorf("empty: %v %+v", err, v)
	}
	if _, err := read("{"); err == nil || !strings.Contains(err.Error(), "could not read the request") {
		t.Errorf("broken: %v", err)
	}
}

// The agent's hooks are refused over 1 MB, not cut: a cut /guard body would not parse and the push would run
// unchecked, so it is stopped; /hook answers 413.
func TestHookBodyOverLimit(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	s, err := term.Start(term.Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh", Token: "tok", Args: []string{"-c", "sleep 5"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	d.Sessions.Put(s)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	big := `{"tool_name":"Bash","tool_input":{"command":"git push","pad":"` + strings.Repeat("x", maxHookBody) + `"}}`

	res, err := http.Post(srv.URL+"/hook?id=a%231&token=tok", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("hook: %d", res.StatusCode)
	}
	u, _ := url.Parse(d.GuardURL("a#1"))
	res, err = http.Post(srv.URL+"/guard?"+u.RawQuery, "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if out["hookSpecificOutput"]["permissionDecision"] != "deny" {
		t.Errorf("guard: %d %v", res.StatusCode, out)
	}
}
