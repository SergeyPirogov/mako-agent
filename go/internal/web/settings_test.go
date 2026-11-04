package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
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

// The effective routes the settings show under the table (stepRoute in settings.html) are the ones core.Step gives a
// started task: every model and effort, in every mode, moved the same way. The page's function is run with node
// (skipped when there is none), with the ladders and shifts /api/settings sends.
func TestSettingsEffectiveRoutes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run the page's script")
	}
	page := string(settingsPage)
	start := strings.Index(page, "function stepRoute(")
	if start < 0 {
		t.Fatal("no stepRoute in settings.html")
	}
	fn := page[start:]
	fn = fn[:strings.Index(fn, "\n}\n")+3]

	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: t.TempDir()}).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var reply struct {
		Routes json.RawMessage `json:"routes"`
	}
	json.NewDecoder(res.Body).Decode(&reply)
	var rc struct {
		Models  []string       `json:"models"`
		Efforts []string       `json:"efforts"`
		Modes   []string       `json:"modes"`
		Shifts  map[string]int `json:"shifts"`
	}
	if err := json.Unmarshal(reply.Routes, &rc); err != nil || len(rc.Shifts) != len(core.Modes) {
		t.Fatalf("/api/settings routes: %v: %s", err, reply.Routes)
	}

	// Every route the table can hold, and ones Mako does not know, with each mode.
	script := fn + `
const rc = ` + string(reply.Routes) + `;
const out = [];
for (const model of [...rc.models, "gpt"]) for (const effort of [...rc.efforts, "turbo"])
  for (const mode of rc.modes) out.push({ model, effort, mode, got: stepRoute({ model, effort }, rc.shifts[mode], rc) });
console.log(JSON.stringify(out));`
	file := filepath.Join(t.TempDir(), "step.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var cases []struct {
		Model, Effort, Mode string
		Got                 struct{ Model, Effort string }
	}
	if err := json.Unmarshal(out, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("node's output: %v: %s", err, out)
	}
	for _, c := range cases {
		want := core.Step(core.Route{Model: c.Model, Effort: c.Effort}, core.ModeShift(c.Mode))
		if c.Got.Model != want.Model || c.Got.Effort != want.Effort {
			t.Errorf("%s · %s in %s: page %s · %s, core.Step %s · %s", c.Model, c.Effort, c.Mode, c.Got.Model, c.Got.Effort, want.Model, want.Effort)
		}
	}
	// The default Work route of a medium task, as the tooltip of a project's mode shows it: sonnet · high.
	if cost := core.Step(core.DefaultRoutes()[core.PhaseWork]["medium"], core.ModeShift(core.ModeCost)); cost.Label() != "sonnet · medium" {
		t.Errorf("cost work/medium: %s", cost.Label())
	}
}

// The mode's hint says when it applies; the table redraws the effective routes on a change of the mode, and a project's
// mode select carries the tooltip with its Work route. The settings have no script tests, so this checks the page's source.
func TestSettingsModeShowsEffectiveRoutes(t *testing.T) {
	page := string(settingsPage)
	for _, want := range []string{
		`addEventListener("change", () => { renderNav(); paintEffective(); })`,
		`data-eff="${p}/${s}"`,
		`data-f="routeMode" title="${esc(routeModeTip(p?.routeMode))}"`,
		`inp.title = routeModeTip(pp.routeMode)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("settings.html lacks %s", want)
		}
	}
	if hint := i18n.T("s.r.modeHint"); !strings.HasPrefix(hint, "Applied when a task starts: each route in the table goes a level down (cost) or up (quality). The table keeps the base values") {
		t.Errorf("modeHint: %q", hint)
	}
	for _, k := range []string{"s.r.effective", "s.r.effectiveHint", "s.pr.routeModeTip"} {
		if i18n.T(k) == k {
			t.Errorf("no %s", k)
		}
	}
}
