package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(`{
	  "executors": {"term": {"type": "terminal"}, "orca": {"type": "orca", "agent": "codex", "mrBy": "me"}},
	  "defaultExecutor": "orca",
	  "trackers": [
	    {"type": "gitlab", "baseUrl": "https://gl.example.com/", "labels": {"trigger": null}, "allProjects": true,
	     "transitions": {"onStart": "Doing", "onMerged": null},
	     "projects": {"grp/app": {}, "grp/two": {"default": "api", "repos": {"api": {"repoPath": "~/code/api"}, "web": {}}}}},
	    {"type": "jira", "jql": "project = PAY", "projects": {"PAY": {"repoPath": "~/code/pay"}}}
	  ]}`), map[string]string{"JIRA_BASE_URL": "https://acme.atlassian.net/"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orca.Agent != "codex" || cfg.Orca.MRBy != "me" || !cfg.Orca.LinkIssue {
		t.Errorf("orca %+v", cfg.Orca)
	}
	gl := cfg.Trackers[0]
	if !gl.TriggerOff || gl.Trigger != "" || !gl.AssignOnTake || !gl.SkipOthersTasks {
		t.Errorf("gitlab %+v", gl)
	}
	if *gl.Transitions.OnStart != "Doing" || *gl.Transitions.OnMerged != "" || gl.Transitions.OnSuccess != nil {
		t.Errorf("transitions %+v", gl.Transitions)
	}
	app := gl.Projects["grp/app"].Repos["app"]
	home, _ := os.UserHomeDir()
	if app.Path != filepath.Join(home, "mako/projects/app") || app.CloneURL != "git@gl.example.com:grp/app.git" {
		t.Errorf("short form %+v", app)
	}
	if gl.Projects["grp/two"].Repos["api"].Path != filepath.Join(home, "code/api") || gl.Projects["grp/two"].Default != "api" {
		t.Errorf("repos %+v", gl.Projects["grp/two"])
	}
	if cfg.Trackers[1].BaseURL != "https://acme.atlassian.net" {
		t.Errorf("jira base %q", cfg.Trackers[1].BaseURL)
	}
}

func TestLoadSharesTheTypeScriptState(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"trackers":[{"type":"github","allProjects":true}]}`), 0o644)
	os.WriteFile(filepath.Join(home, ".env"), []byte("GITHUB_TOKEN=abc\n"), 0o644)
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != filepath.Join(home, ".mako") || cfg.Get("GITHUB_TOKEN") != "abc" {
		t.Errorf("state %s token %q", cfg.StateDir, cfg.Get("GITHUB_TOKEN"))
	}
}

func TestLoadWithoutConfig(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != ErrMissing || cfg == nil || cfg.Notifications.Port != 7777 || len(cfg.Trackers) != 0 {
		t.Fatalf("%v %+v", err, cfg)
	}
}

func TestSaveEnv(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".env"), []byte("# tokens\nGITLAB_TOKEN=old\nOTHER=1\n"), 0o600)
	if err := SaveEnv(home, map[string]string{"GITLAB_TOKEN": "new", "JIRA_EMAIL": "a@b", "OTHER": ""}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".env"))
	if string(data) != "# tokens\nGITLAB_TOKEN=new\nJIRA_EMAIL=a@b\n" {
		t.Fatalf("%q", data)
	}
}

// A .env left 0644 (by hand, by an older Mako) is 0600 after the save: it holds the tokens.
func TestSaveEnvTightensRights(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".env")
	os.WriteFile(path, []byte("GITHUB_TOKEN=abc\n"), 0o644)
	if err := SaveEnv(home, map[string]string{"GITLAB_TOKEN": "x"}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf(".env mode = %o, want 600", perm)
	}
}

// What SaveEnv writes is read back as it was typed: spaces, #, quotes and backslashes included.
func TestSaveEnvRoundTrip(t *testing.T) {
	home := t.TempDir()
	values := map[string]string{
		"SPACES": "a b  c", "HASH": "x#y", "DOUBLE": `say "hi"`, "SINGLE": "it's", "QUOTED": `"q"`,
		"SLASH": `back\slash\`, "LEAD": "  lead", "PLAIN": "glpat-abc_123.x/y:z@h+=,", "EQ": "a=b",
	}
	if err := SaveEnv(home, values); err != nil {
		t.Fatal(err)
	}
	got := readEnv(filepath.Join(home, ".env"))
	for k, v := range values {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	data, _ := os.ReadFile(filepath.Join(home, ".env"))
	if !strings.Contains(string(data), "PLAIN=glpat-abc_123.x/y:z@h+=,\n") {
		t.Fatalf("a plain value got quoted:\n%s", data)
	}
	// Saved again over itself, nothing changes.
	if err := SaveEnv(home, map[string]string{"SPACES": values["SPACES"]}); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(home, ".env")); string(again) != string(data) {
		t.Fatalf("changed on a second save:\n%s\n---\n%s", data, again)
	}
}

// A value on several lines would break the file: it is refused and .env stays as it was.
func TestSaveEnvRefusesNewlines(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".env")
	os.WriteFile(path, []byte("GITHUB_TOKEN=abc\n"), 0o600)
	for _, v := range []string{"a\nINJECTED=1", "a\rb"} {
		if err := SaveEnv(home, map[string]string{"GITHUB_TOKEN": v}); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Fatalf("%q: err = %v", v, err)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "GITHUB_TOKEN=abc\n" {
		t.Fatalf(".env = %q", data)
	}
}

// A .env written by hand reads as before: quotes around a value are taken off, the rest as it stands.
func TestReadEnvByHand(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# tokens\nA=v\nB=\"v\"\nC='v'\n D = v \nE=C:\\path\nnot a pair\n"), 0o600)
	got := readEnv(path)
	for _, k := range []string{"A", "B", "C", "D"} {
		if got[k] != "v" {
			t.Errorf("%s = %q, want v", k, got[k])
		}
	}
	if got["E"] != `C:\path` || len(got) != 5 {
		t.Fatalf("%v", got)
	}
}

func TestParseTerminal(t *testing.T) {
	c, err := Parse([]byte(`{"defaultExecutor":"t","executors":{
		"o":{"type":"orca","mrBy":"me"},
		"t":{"type":"terminal","command":"~/bin/claude","allowedTools":["Bash(git:*)"]}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Executor != "terminal" || c.Terminal.Command != "~/bin/claude" || c.Terminal.PermissionMode != "auto" ||
		len(c.Terminal.AllowedTools) != 1 || c.Orca.MRBy != "me" {
		t.Fatalf("%+v %+v", c.Terminal, c.Orca)
	}
	if c.Terminal.AutoRebase != "" {
		t.Fatalf("auto-rebase on by default: %q", c.Terminal.AutoRebase)
	}
	for raw, want := range map[string]string{"merge": "merge", "rebase": "rebase", "squash": ""} {
		c, _ := Parse([]byte(`{"executors":{"t":{"type":"terminal","autoRebase":"`+raw+`","rebaseEveryMin":15}}}`), nil)
		if c.Terminal.AutoRebase != want || c.Terminal.RebaseEveryMin != 15 {
			t.Fatalf("%s: %+v", raw, c.Terminal)
		}
	}
	if c, _ := Parse([]byte(`{"executors":{"o":{"type":"orca"}}}`), nil); c.Executor != "orca" {
		t.Fatalf("executor %q", c.Executor)
	}
}

func TestLimitPausePct(t *testing.T) {
	for raw, want := range map[string]int{`{}`: 85, `{"limitPausePct": 70}`: 70, `{"limitPausePct": 0}`: 0} {
		if c, err := Parse([]byte(raw), nil); err != nil || c.LimitPausePct != want {
			t.Fatalf("%s: %v %v", raw, c.LimitPausePct, err)
		}
	}
}

func TestParseDigest(t *testing.T) {
	cfg, err := Parse([]byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.MorningMin != 9*60 || cfg.Notifications.AwayHours != 4 {
		t.Fatalf("defaults %+v", cfg.Notifications)
	}
	cfg, err = Parse([]byte(`{"notifications": {"morningDigest": "7:30", "awayHours": 0}}`), nil)
	if err != nil || cfg.Notifications.MorningMin != 7*60+30 || cfg.Notifications.AwayHours != 0 {
		t.Fatalf("set %+v %v", cfg.Notifications, err)
	}
	if cfg, err = Parse([]byte(`{"notifications": {"morningDigest": "-"}}`), nil); err != nil || cfg.Notifications.MorningMin != -1 {
		t.Fatalf("off %+v %v", cfg, err)
	}
	for _, bad := range []string{"25:00", "ранок", "9"} {
		if _, err := Parse([]byte(`{"notifications": {"morningDigest": "`+bad+`"}}`), nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseReviewer(t *testing.T) {
	// The settings of older versions (the reviewer on by itself, its rounds, by risk) are ignored.
	c, _ := Parse([]byte(`{"agentReview":{"enabled":true,"maxRounds":3,"when":"high","model":" opus "}}`), nil)
	if c.Reviewer.Model != "opus" || c.Reviewer.Comments != "" {
		t.Fatalf("reviewer %+v", c.Reviewer)
	}
	// Its comments in the tracker: off unless asked for, and only where they can go.
	for v, want := range map[string]string{"issue": "issue", " mr ": "mr", "both": "both", "jira": "", "": ""} {
		if c, _ := Parse([]byte(`{"agentReview":{"comments":"`+v+`"}}`), nil); c.Reviewer.Comments != want {
			t.Errorf("comments %q: %q", v, c.Reviewer.Comments)
		}
	}
}

func TestParseCodeModel(t *testing.T) {
	if c, _ := Parse([]byte(`{}`), nil); c.CodeModel != DefaultCodeModel {
		t.Fatalf("default %q", c.CodeModel)
	}
	for _, m := range []string{"opus", "claude-opus-5-5", "claude-sonnet-5-5[1m]"} {
		if c, err := Parse([]byte(`{"codeAsk":{"model":" `+m+` "}}`), nil); err != nil || c.CodeModel != m {
			t.Fatalf("picked %q: %v", m, err)
		}
	}
	// It goes to claude's --model: never an option of its own, several words or a quote.
	for _, m := range []string{"--dangerously-skip-permissions", "opus --tools Bash", "-x", "a\"b", "a/b"} {
		if _, err := Parse([]byte(`{"codeAsk":{"model":`+strconv.Quote(m)+`}}`), nil); err == nil {
			t.Fatalf("%q accepted", m)
		}
	}
}

func TestParseNotificationEvents(t *testing.T) {
	cfg, err := Parse([]byte(`{}`), nil)
	if err != nil || cfg.Notifications.Muted("newTask") {
		t.Fatalf("by default every kind is shown: %+v %v", cfg.Notifications, err)
	}
	cfg, err = Parse([]byte(`{"notifications": {"events": {"newTask": false, "merged": true}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.Muted("newTask") || cfg.Notifications.Muted("merged") || cfg.Notifications.Muted("waiting") {
		t.Fatalf("off %+v", cfg.Notifications.Off)
	}
}

func TestAutoWip(t *testing.T) {
	const gb = 1 << 30
	for _, c := range []struct {
		cores int
		mem   uint64
		want  int
	}{
		{10, 16 * gb, 4}, // memory bounds it
		{8, 64 * gb, 4},  // cores do
		{12, 0, 6},       // memory unknown: cores alone
		{1, 2 * gb, 1},   // never below one
	} {
		if got := autoWip(c.cores, c.mem); got != c.want {
			t.Errorf("autoWip(%d, %d GB) = %d, want %d", c.cores, c.mem/gb, got, c.want)
		}
	}
	cfg, err := Parse([]byte(`{"wipLimit": 99, "wipAuto": true}`), nil)
	if err != nil || !cfg.WipAuto || cfg.WipLimit != AutoWip() {
		t.Fatalf("wipAuto: %+v %v", cfg, err)
	}
}

func TestParseTrackerPaused(t *testing.T) {
	c, err := Parse([]byte(`{"trackers":[{"type":"github","allProjects":true,"paused":true},{"type":"gitlab","allProjects":true}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Trackers[0].Paused || c.Trackers[1].Paused {
		t.Fatalf("paused: %v %v", c.Trackers[0].Paused, c.Trackers[1].Paused)
	}
}

func TestParseEstimate(t *testing.T) {
	if c, _ := Parse([]byte(`{}`), nil); !c.Estimate || !c.EstimateModel {
		t.Fatal("the estimate and its model are on by default")
	}
	if c, _ := Parse([]byte(`{"estimate":{"useModel":false}}`), nil); c.EstimateModel {
		t.Fatal("the estimate's model turned off")
	}
	if c, _ := Parse([]byte(`{"estimate":{"enabled":false}}`), nil); c.Estimate {
		t.Fatal("turned off")
	}
}

func TestChecks(t *testing.T) {
	cfg, err := Parse([]byte(`{
	  "executors": {"term": {"type": "terminal", "checkTimeoutSec": 90}},
	  "trackers": [{"type": "github", "projects": {"acme/web": {"checks": ["go vet ./...", "  ", " go test ./... "]}, "acme/api": {}}}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	web := cfg.Trackers[0].Projects["acme/web"].Repos["web"]
	if len(web.Checks) != 2 || web.Checks[1] != "go test ./..." {
		t.Errorf("checks %q", web.Checks)
	}
	if api := cfg.Trackers[0].Projects["acme/api"].Repos["api"]; len(api.Checks) != 0 {
		t.Errorf("no checks: %q", api.Checks)
	}
	if cfg.Terminal.CheckTimeout().Seconds() != 90 || cfg.Terminal.CheckTries() != 3 {
		t.Errorf("timeout %s tries %d", cfg.Terminal.CheckTimeout(), cfg.Terminal.CheckTries())
	}
}

func TestParseAutoTakeOnly(t *testing.T) {
	for in, want := range map[string]string{"": AutoTakeAll, "all": AutoTakeAll, "smallClear": AutoTakeSmallClear, "small": AutoTakeSmall, "big": AutoTakeAll} {
		if c, _ := Parse([]byte(`{"notifications":{"autoTake":true,"autoTakeOnly":"`+in+`"}}`), nil); c.Notifications.AutoTakeOnly != want || !c.Notifications.AutoTake {
			t.Errorf("autoTakeOnly %q: %+v, want %q", in, c.Notifications, want)
		}
	}
}

// The Mac stays awake while agents work unless the settings say otherwise.
func TestKeepAwake(t *testing.T) {
	if c, _ := Parse([]byte(`{}`), nil); !c.KeepAwake {
		t.Error("on by default")
	}
	if c, _ := Parse([]byte(`{"keepAwake":false}`), nil); c.KeepAwake {
		t.Error("off when the settings say")
	}
}

// The routes: the defaults, with a cell changed; what Mako does not know keeps the default; a repo's own mode.
func TestParseRoutes(t *testing.T) {
	c, _ := Parse([]byte(`{}`), nil)
	if c.RouteMode != core.ModeBalanced || c.Routes[core.PhaseWork]["big"] != (core.Route{Model: "opus", Effort: "high"}) {
		t.Fatalf("defaults: %s %+v", c.RouteMode, c.Routes)
	}
	c, _ = Parse([]byte(`{"routes":{"mode":"cost","table":{"work":{"big":{"model":"sonnet"},"small":{"model":"gpt","effort":"xhigh"}},"nope":{"big":{"model":"opus"}},"plan":{"huge":{"model":"opus"}}}}}`), nil)
	if c.RouteMode != core.ModeCost {
		t.Fatalf("mode %q", c.RouteMode)
	}
	if c.Routes[core.PhaseWork]["big"] != (core.Route{Model: "sonnet", Effort: "high"}) || c.Routes[core.PhaseWork]["small"] != (core.Route{Model: "sonnet", Effort: "xhigh"}) {
		t.Fatalf("work: %+v", c.Routes[core.PhaseWork])
	}
	if _, ok := c.Routes["nope"]; ok || len(c.Routes[core.PhasePlan]) != 3 {
		t.Fatalf("unknown phase or size: %+v", c.Routes)
	}
	if core.DefaultRoutes()[core.PhaseWork]["big"].Model != "opus" {
		t.Fatal("the defaults were changed")
	}
	if c, _ := Parse([]byte(`{"routes":{"mode":"fast"}}`), nil); c.RouteMode != core.ModeBalanced {
		t.Fatalf("unknown mode %q", c.RouteMode)
	}
	c, err := Parse([]byte(`{"trackers":[{"type":"gitlab","projects":{"grp/app":{"routeMode":"quality"},"grp/web":{}}}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Trackers[0].Projects["grp/app"].Repos["app"].RouteMode != core.ModeQuality || c.Trackers[0].Projects["grp/web"].Repos["web"].RouteMode != "" {
		t.Fatalf("repo modes: %+v %+v", c.Trackers[0].Projects["grp/app"].Repos["app"], c.Trackers[0].Projects["grp/web"].Repos["web"])
	}
}

// The prices: the defaults, with a kind changed by the config; a negative one is ignored.
func TestParsePrices(t *testing.T) {
	c, _ := Parse([]byte(`{}`), nil)
	if c.Prices["opus"] != core.DefaultPrices()["opus"] {
		t.Fatalf("defaults %+v", c.Prices)
	}
	c, _ = Parse([]byte(`{"routes":{"prices":{"claude-opus-4-7":{"input":5,"output":25,"cacheRead":-1},"gpt-5":{"input":1}}}}`), nil)
	if _, ok := c.Prices["gpt-5"]; ok || len(c.Prices) != len(core.DefaultPrices()) {
		t.Fatalf("a family with no price is ignored: %+v", c.Prices)
	}
	want := core.DefaultPrices()["opus"]
	want.Input, want.Output = 5, 25
	if c.Prices["opus"] != want || c.Prices["sonnet"] != core.DefaultPrices()["sonnet"] {
		t.Fatalf("changed %+v", c.Prices)
	}
}
