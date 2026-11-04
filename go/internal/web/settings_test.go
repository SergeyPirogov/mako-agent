package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Saving a tracker whose «Лише призначені на» names an unknown login says so.
func TestAssigneeProblems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/SergeyPirogov" {
			w.Write([]byte(`{"login":"SergeyPirogov"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	tracker := func(login string) trackers.Tracker {
		gh, _ := trackers.NewGitHub(&config.Tracker{Type: "github", ID: "github", APIURL: srv.URL, AllProjects: true, Assignee: login}, "x")
		return gh
	}
	if got := assigneeProblems(context.Background(), []trackers.Tracker{tracker("SergeyPirogov"), tracker("me")}); len(got) != 0 {
		t.Errorf("known: %q", got)
	}
	got := assigneeProblems(context.Background(), []trackers.Tracker{tracker("SergeyPirgov")})
	if len(got) != 1 || !strings.Contains(got[0], "«SergeyPirgov» does not exist") {
		t.Errorf("unknown: %q", got)
	}
}

// The header's ☕ writes «keepAwake» into the config and Mako works with it at once.
func TestAwakeToggle(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"wipLimit":3}`), 0o644)
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), KeepAwake: true}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	post := func(on string) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/awake?on="+on, nil)
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("awake=%s: %v %v", on, err, res)
		}
	}
	post("0")
	b, _ := os.ReadFile(filepath.Join(home, "mako.config.json"))
	if !strings.Contains(string(b), `"keepAwake": false`) || !strings.Contains(string(b), `"wipLimit": 3`) || d.Config().KeepAwake {
		t.Fatalf("off: %s, %v", b, d.Config().KeepAwake)
	}
	post("1")
	b, _ = os.ReadFile(filepath.Join(home, "mako.config.json"))
	if strings.Contains(string(b), "keepAwake") || !d.Config().KeepAwake {
		t.Fatalf("on: %s", b)
	}
}

// ☕, a tracker's pause and the code model changed at once: each change stays, none fails on the file's rename.
func TestConfigChangesAtOnce(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".env"), []byte("GITHUB_TOKEN=a\n"), 0o600)
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"wipLimit":3,"trackers":[{"type":"github","allProjects":true}]}`), 0o644)
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), KeepAwake: true}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	posts := []struct{ path, body string }{
		{"/api/awake?on=0", ""},
		{"/api/tracker-pause?id=github&on=1", ""},
		{"/api/code/model", `{"model":"opus"}`},
	}
	var wg sync.WaitGroup
	errs := make(chan string, 3*len(posts))
	for range 3 {
		for _, p := range posts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("POST", srv.URL+p.path, strings.NewReader(p.body))
				req.Header.Set("Origin", srv.URL)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err.Error()
					return
				}
				var out map[string]any
				json.NewDecoder(res.Body).Decode(&out)
				res.Body.Close()
				if out["ok"] != true {
					errs <- fmt.Sprintf("%s: %v", p.path, out)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	b, _ := os.ReadFile(filepath.Join(home, "mako.config.json"))
	var cfg struct {
		WipLimit  int                     `json:"wipLimit"`
		KeepAwake *bool                   `json:"keepAwake"`
		CodeAsk   struct{ Model string }  `json:"codeAsk"`
		Trackers  []struct{ Paused bool } `json:"trackers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config: %v\n%s", err, b)
	}
	if cfg.WipLimit != 3 || cfg.KeepAwake == nil || *cfg.KeepAwake || cfg.CodeAsk.Model != "opus" || len(cfg.Trackers) != 1 || !cfg.Trackers[0].Paused {
		t.Fatalf("a change got lost:\n%s", b)
	}
}

// No mako.config.json yet: ☕ writes one.
func TestAwakeWithoutConfig(t *testing.T) {
	home := t.TempDir()
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), KeepAwake: true}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/awake?on=0", nil)
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if b, _ := os.ReadFile(filepath.Join(home, "mako.config.json")); out["ok"] != true || !strings.Contains(string(b), `"keepAwake": false`) {
		t.Fatalf("%v: %s", out, b)
	}
}

// The pages get their words in English, whatever the agent language.
func TestI18n(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), AgentLanguage: "uk"}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: t.TempDir()}).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/i18n")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var words struct {
		Strings map[string]string `json:"strings"`
	}
	json.NewDecoder(res.Body).Decode(&words)
	if words.Strings["s.save"] != "Save" {
		t.Fatalf("en: %q", words.Strings["s.save"])
	}
}

// An older config's «language» is saved as «agentLanguage», and settings read it back that way.
func TestSaveAgentLanguage(t *testing.T) {
	home := t.TempDir()
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/settings", strings.NewReader(`{"config": {"language": "uk"}, "env": {}}`))
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("save: %v %v", err, res)
	}
	if d.Config().AgentLanguage != "uk" || i18n.AgentLanguage() != i18n.UK {
		t.Fatalf("agent language %q / %q", d.Config().AgentLanguage, i18n.AgentLanguage())
	}
	b, _ := os.ReadFile(filepath.Join(home, "mako.config.json"))
	if !strings.Contains(string(b), `"agentLanguage": "uk"`) || strings.Contains(string(b), `"language"`) {
		t.Fatalf("saved: %s", b)
	}
}

// Removing worktrees and opening them in Finder is for the board's own page only, and a body that is no JSON is
// refused.
func TestWorktreeRemoveEndpoints(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: t.TempDir()}).Handler())
	defer srv.Close()
	post := func(path, origin, body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, p := range []string{"/api/settings/worktrees/remove", "/api/settings/worktrees/open"} {
		if res := post(p, "https://evil.example", `{}`); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s from another site: %d", p, res.StatusCode)
		}
		if res := post(p, srv.URL, `not json`); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s with a bad body: %d", p, res.StatusCode)
		}
	}
	res := post("/api/settings/worktrees/remove", srv.URL, `{"items":[{"repo":"x","path":"/tmp/x"}],"force":true}`)
	var out dispatch.RemoveResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 || out.Removed != 0 || len(out.Errors) != 1 {
		t.Fatalf("unknown project: %d %+v %v", res.StatusCode, out, err)
	}
}

// On the first start the «Add a tracker» pane shows the tracker tiles first; the language card follows them.
func TestSettingsFirstRunOrder(t *testing.T) {
	page := string(settingsPage)
	pane := page[strings.Index(page, `data-pane="add"`):]
	pane = pane[:strings.Index(pane, `data-pane="language"`)]
	title, tiles, lang := strings.Index(pane, `data-t="s.add.title"`), strings.Index(pane, `class="add-big"`), strings.Index(pane, `id="first-language"`)
	if title < 0 || tiles < 0 || lang < 0 || !(title < tiles && tiles < lang) {
		t.Fatalf("order in the add pane: title %d, tiles %d, language %d", title, tiles, lang)
	}
}

// An offered lesson's rule is shown whole, in a multi-line field (not a text input that cuts a long rule), and
// «Save to CLAUDE.md» sends what that field holds. The settings have no script tests, so this checks the page's source.
func TestLessonRuleIsTextarea(t *testing.T) {
	page := string(settingsPage)
	start := strings.Index(page, `class="lesson-offer"`)
	if start < 0 {
		t.Fatal("no offered lesson in settings.html")
	}
	offer := page[start:]
	offer = offer[:strings.Index(offer, `data-answer="base"`)]
	if !strings.Contains(offer, `<textarea rows="3" aria-label=`) || !strings.Contains(offer, `>${esc(l.rule)}</textarea>`) {
		t.Errorf("the lesson's rule is not a three-row textarea: %s", offer)
	}
	if strings.Contains(offer, `<input`) {
		t.Error("the lesson's rule is still in an input")
	}
	if !strings.Contains(page, `box.querySelector("textarea").value`) || strings.Contains(page, `box.querySelector("input")`) {
		t.Error("«Save to CLAUDE.md» does not read the rule from the textarea")
	}
	if !strings.Contains(page, ".lesson-offer textarea { min-height: 0;") {
		t.Error("the lesson's textarea does not reset the global textarea's min-height")
	}
}
