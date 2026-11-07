package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// The buttons of «New idea»: from the board of this Mac only; each answers what went wrong; the board gets the
// ideas with the state.
func TestSpecsRoutes(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	post := func(action, body, origin string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/specs/"+action, strings.NewReader(body))
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if code, _ := post("drop", `{"id":"spec-1"}`, "https://evil.example"); code != 403 {
		t.Errorf("cross-site: %d", code)
	}
	if code, _ := post("nope", `{"id":"spec-1"}`, srv.URL); code != 404 {
		t.Errorf("unknown action: %d", code)
	}
	for action, want := range map[string]string{
		"new":    "write the idea",
		"answer": "no such idea",
		"retry":  "no such idea",
		"create": "no such idea",
		"drop":   "no such idea",
	} {
		if code, out := post(action, `{"id":"spec-1"}`, srv.URL); code != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), want) {
			t.Errorf("%s: %d %v, want %q", action, code, out, want)
		}
	}
	// A tracker that cannot open tasks: the idea is refused before the agent is asked.
	if _, out := post("new", `{"tracker":"gh","project":"me/app","text":"Експорт у CSV"}`, srv.URL); out["ok"] != false || !strings.Contains(out["error"].(string), "no tracker") {
		t.Errorf("new without a tracker: %v", out)
	}
	if _, out := post("new", `{"text":"`+strings.Repeat("x", specBodyMax)+`"}`, srv.URL); out["ok"] != false || out["error"] != errTooLong {
		t.Errorf("a body over the limit: %v", out)
	}
	if !macOnly("/api/specs/new") || !macOnly("/api/specs/create") {
		t.Error("the agent writing a specification runs from a phone")
	}

	res, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st map[string]any
	json.NewDecoder(res.Body).Decode(&st)
	if specs, ok := st["specs"].([]any); !ok || len(specs) != 0 {
		t.Errorf("state's specs: %v", st["specs"])
	}
}
