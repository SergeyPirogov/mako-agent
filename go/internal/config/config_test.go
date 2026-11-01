package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
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
