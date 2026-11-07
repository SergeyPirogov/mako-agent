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

// The buttons of «For Mako»: from the board only, with a key; each answers what went wrong; the board gets the
// suggestions with the state.
func TestIdeasRoutes(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	post := func(action, body, origin string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/ideas/"+action, strings.NewReader(body))
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
	if code, _ := post("no", `{"key":"fault:abc"}`, "https://evil.example"); code != 403 {
		t.Errorf("cross-site: %d", code)
	}
	if code, _ := post("nope", `{"key":"fault:abc"}`, srv.URL); code != 404 {
		t.Errorf("unknown action: %d", code)
	}
	if code, _ := post("no", `{}`, srv.URL); code != 400 {
		t.Errorf("no key: %d", code)
	}
	if code, _ := post("no", `{"key":"`+strings.Repeat("x", 1<<17)+`"}`, srv.URL); code != 400 {
		t.Errorf("a body over the limit: %d", code)
	}
	if code, out := post("no", `{"key":"fault:abc"}`, srv.URL); code != 200 || out["ok"] != true {
		t.Errorf("no: %d %v", code, out)
	}
	if code, out := post("no", `{"key":"other"}`, srv.URL); code != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), "unknown suggestion") {
		t.Errorf("no, unknown key: %d %v", code, out)
	}
	if code, out := post("file", `{"key":"fault:gone","target":"mako"}`, srv.URL); code != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), "gone") {
		t.Errorf("file a suggestion that is not there: %d %v", code, out)
	}

	res, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st map[string]any
	json.NewDecoder(res.Body).Decode(&st)
	if ideas, ok := st["ideas"].([]any); !ok || len(ideas) != 0 {
		t.Errorf("ideas in the state: %v", st["ideas"])
	}
}
