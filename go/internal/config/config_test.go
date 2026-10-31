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
