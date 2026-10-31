package config

import (
	"os"
	"path/filepath"
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
	if c, _ := Parse([]byte(`{}`), nil); c.Reviewer.Enabled || c.Reviewer.MaxRounds != 1 {
		t.Fatalf("default %+v", c.Reviewer)
	}
	c, _ := Parse([]byte(`{"agentReview":{"enabled":true,"maxRounds":3,"model":" opus "}}`), nil)
	if !c.Reviewer.Enabled || c.Reviewer.MaxRounds != 3 || c.Reviewer.Model != "opus" {
		t.Fatalf("reviewer %+v", c.Reviewer)
	}
	if c, _ := Parse([]byte(`{"agentReview":{"maxRounds":0}}`), nil); c.Reviewer.MaxRounds != 1 {
		t.Fatalf("0 rounds: %+v", c.Reviewer)
	}
	for in, want := range map[string]string{"": ReviewAlways, "medium": ReviewMedium, "high": ReviewHigh, "low": ReviewAlways} {
		if c, _ := Parse([]byte(`{"agentReview":{"when":"`+in+`"}}`), nil); c.Reviewer.When != want {
			t.Errorf("when %q: %q, want %q", in, c.Reviewer.When, want)
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
