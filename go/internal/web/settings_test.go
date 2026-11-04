package web

import (
	"context"
	"encoding/json"
	"errors"
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

// «Продовжувати після скидання ліміту» is on unless the settings say off, and saving it changes it at once.
func TestSaveResumeAfterLimit(t *testing.T) {
	home := t.TempDir()
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	save := func(cfg string) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/settings", strings.NewReader(`{"config": `+cfg+`, "env": {}}`))
		req.Header.Set("Origin", srv.URL)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("save %s: %v %v", cfg, err, res)
		}
	}
	save(`{}`)
	if !d.Config().ResumeAfterLimit {
		t.Fatal("off by default")
	}
	save(`{"resumeAfterLimit": false}`)
	b, _ := os.ReadFile(filepath.Join(home, "mako.config.json"))
	if d.Config().ResumeAfterLimit || !strings.Contains(string(b), `"resumeAfterLimit": false`) {
		t.Fatalf("off: %v, %s", d.Config().ResumeAfterLimit, b)
	}
	res, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Config map[string]any `json:"config"`
	}
	json.NewDecoder(res.Body).Decode(&got)
	if got.Config["resumeAfterLimit"] != false {
		t.Fatalf("settings read back %v", got.Config)
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

// jsFunc: the source of the page's top-level function name, up to its closing brace at the line start.
func jsFunc(t *testing.T, page, name string) string {
	t.Helper()
	start := strings.Index(page, "function "+name+"(")
	if start < 0 {
		t.Fatalf("no function %s in the page", name)
	}
	fn := page[start:]
	return fn[:strings.Index(fn, "\n}\n")+3]
}

// runNode runs script with node and decodes what it prints (JSON); the test is skipped when there is no node.
func runNode(t *testing.T, script string, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run the page's script")
	}
	file := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(node, file).Output()
	if ee := (&exec.ExitError{}); errors.As(err, &ee) {
		t.Fatalf("node: %v: %s", err, ee.Stderr)
	} else if err != nil {
		t.Fatalf("node: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("node's output: %v: %s", err, b)
	}
}

// routesReply: the routes part of /api/settings, as the page gets it.
func routesReply(t *testing.T) json.RawMessage {
	t.Helper()
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
		Shifts map[string]int `json:"shifts"`
	}
	if err := json.Unmarshal(reply.Routes, &rc); err != nil || len(rc.Shifts) != len(core.Modes) {
		t.Fatalf("/api/settings routes: %v: %s", err, reply.Routes)
	}
	return reply.Routes
}

type jsRoute struct{ Model, Effort string }

// The effective routes the settings show under the table (stepRoute and effectiveRoute in settings.html) are the ones
// core.Step gives a started task: every model and effort, in every mode, moved the same way, and with the table's own
// cells over the defaults. The page's functions are run with node (skipped when there is none), with the ladders and
// shifts /api/settings sends.
func TestSettingsEffectiveRoutes(t *testing.T) {
	page := string(settingsPage)
	routes := routesReply(t)
	var rc struct {
		Models  []string `json:"models"`
		Efforts []string `json:"efforts"`
		Modes   []string `json:"modes"`
	}
	json.Unmarshal(routes, &rc)

	script := jsFunc(t, page, "stepRoute") + jsFunc(t, page, "effectiveRoute") + `
let routeChoices = ` + string(routes) + `;
let cfg = { routes: { table: { work: { medium: { model: "opus" }, big: { effort: "max" } } } } };
const out = { step: [], eff: [] };
for (const model of [...routeChoices.models, "gpt"]) for (const effort of [...routeChoices.efforts, "turbo"])
  for (const mode of routeChoices.modes) out.step.push({ model, effort, mode, got: stepRoute({ model, effort }, routeChoices.shifts[mode], routeChoices) });
for (const phase of routeChoices.phases) for (const size of routeChoices.sizes) for (const mode of routeChoices.modes)
  out.eff.push({ phase, size, mode, got: effectiveRoute(phase, size, mode) });
console.log(JSON.stringify(out));`
	var out struct {
		Step []struct {
			Model, Effort, Mode string
			Got                 jsRoute
		}
		Eff []struct {
			Phase, Size, Mode string
			Got               jsRoute
		}
	}
	runNode(t, script, &out)
	if len(out.Step) == 0 || len(out.Eff) == 0 {
		t.Fatal("node gave nothing")
	}
	for _, c := range out.Step {
		want := core.Step(core.Route{Model: c.Model, Effort: c.Effort}, core.ModeShift(c.Mode))
		if c.Got.Model != want.Model || c.Got.Effort != want.Effort {
			t.Errorf("%s · %s in %s: page %s · %s, core.Step %s · %s", c.Model, c.Effort, c.Mode, c.Got.Model, c.Got.Effort, want.Model, want.Effort)
		}
	}
	// The table as the page's cfg holds it: the defaults, with work/medium's model and work/big's effort changed.
	table := core.DefaultRoutes()
	table[core.PhaseWork]["medium"] = core.Route{Model: "opus", Effort: table[core.PhaseWork]["medium"].Effort}
	table[core.PhaseWork]["big"] = core.Route{Model: table[core.PhaseWork]["big"].Model, Effort: "max"}
	for _, c := range out.Eff {
		want := core.Step(table[c.Phase][c.Size], core.ModeShift(c.Mode))
		if c.Got.Model != want.Model || c.Got.Effort != want.Effort {
			t.Errorf("%s/%s in %s: page %s · %s, core.Step %s · %s", c.Phase, c.Size, c.Mode, c.Got.Model, c.Got.Effort, want.Model, want.Effort)
		}
	}
}

// A project's mode select tells the Work route of a medium task under its mode, and the global mode's when they differ;
// "as set" tells the global one's only. Run with node, with the page's strings.
func TestSettingsRouteModeTip(t *testing.T) {
	page := string(settingsPage)
	words := map[string]string{}
	for _, k := range []string{"s.pr.routeModeTip", "s.pr.routeModeVs", "s.r.mode.cost", "s.r.mode.balanced", "s.r.mode.quality"} {
		words[k] = i18n.T(k)
		if words[k] == k {
			t.Fatalf("no string %s", k)
		}
	}
	w, _ := json.Marshal(words)
	script := jsFunc(t, page, "stepRoute") + jsFunc(t, page, "effectiveRoute") + jsFunc(t, page, "routeText") + jsFunc(t, page, "routeModeTip") + `
const I18N = ` + string(w) + `;
const t = (key, vars = {}) => String(I18N[key] ?? key).replace(/\{(\w+)\}/g, (m, k) => (k in vars ? vars[k] : m));
let routeChoices = ` + string(routesReply(t)) + `;
const out = {};
let cfg;
for (const global of ["", "cost", "quality"]) {
  cfg = { routes: global ? { mode: global } : {} };
  for (const own of ["", "cost", "balanced", "quality"]) out[global + "/" + own] = routeModeTip(own);
}
console.log(JSON.stringify(out));`
	var tips map[string]string
	runNode(t, script, &tips)
	for key, got := range tips {
		global, own, _ := strings.Cut(key, "/")
		if global == "" {
			global = core.ModeBalanced
		}
		if own == "" {
			own = global
		}
		work := func(mode string) string {
			return core.Step(core.DefaultRoutes()[core.PhaseWork]["medium"], core.ModeShift(mode)).Label()
		}
		want := "Work, medium task: " + work(own) + " (" + own + ")"
		if own != global {
			want += "; the global mode: " + work(global) + " (" + global + ")"
		}
		if got != want {
			t.Errorf("tip of %q: %q, want %q", key, got, want)
		}
	}
	if len(tips) != 12 {
		t.Errorf("%d tips", len(tips))
	}
}

// The board card's label of the agent's route (routeLabel in board.html) is core.Route.LabelWithMode's, for every mode,
// an unknown one too.
func TestBoardRouteLabel(t *testing.T) {
	board, err := os.ReadFile("board.html")
	if err != nil {
		t.Fatal(err)
	}
	cases := []core.Route{
		{Model: "opus", Effort: "high"},
		{Model: "opus", Effort: "high", Mode: core.ModeBalanced},
		{Model: "opus", Effort: "high", Mode: core.ModeCost},
		{Model: "opus", Effort: "xhigh", Mode: core.ModeQuality},
		{Model: "opus", Mode: core.ModeQuality},
		{Effort: "low", Mode: core.ModeCost},
		{Mode: core.ModeQuality},
		{Model: "opus", Effort: "high", Mode: "turbo"},
	}
	in, _ := json.Marshal(cases)
	var got []string
	runNode(t, jsFunc(t, string(board), "routeLabel")+"\nconsole.log(JSON.stringify("+string(in)+".map(routeLabel)));", &got)
	if len(got) != len(cases) {
		t.Fatalf("labels: %q", got)
	}
	for i, r := range cases {
		if got[i] != r.LabelWithMode() {
			t.Errorf("%+v: board %q, Go %q", r, got[i], r.LabelWithMode())
		}
	}
}

// The mode's hint says when it applies. (That the table redraws on a change of the mode is in the page's script, which
// has no browser test: checked by hand, see the MR.)
func TestSettingsModeHint(t *testing.T) {
	if hint := i18n.T("s.r.modeHint"); !strings.HasPrefix(hint, "Applied when a task starts: each route in the table goes a level down (cost) or up (quality). The table keeps the base values") {
		t.Errorf("modeHint: %q", hint)
	}
}
