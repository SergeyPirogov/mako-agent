package config

import (
	"os"
	"reflect"
	"slices"
	"testing"
)

const examplePath = "../../../mako.config.example.json"

func TestUnknownKeys(t *testing.T) {
	got := UnknownKeys([]byte(`{
	  "pollIntervalSec": 30, "maxConcurrent": 2, "Language": "uk",
	  "notifications": {"timeoutSec": 600, "events": {"anyKind": false}},
	  "agentReview": {"enabled": true, "model": "opus"},
	  "routes": {"table": {"work": {"big": {"model": "opus", "Effort": "high", "speed": 1}}}},
	  "comments": {"onStart": "plain", "onReview": {"en": "x", "de": "y"}},
	  "executors": {"claude": {"type": "claude-headless", "args": ["--x"], "label": "c"}},
	  "trackers": [
	    {"type": "jira", "labels": {"trigger": null, "other": 1}, "transitions": {"onStart": "A", "onDone": "B"},
	     "projects": {
	       "P": {"default": "a", "repos": {"a": {"repoPath": "/x", "match": {}}}, "typo": 1},
	       "Q": {"repoPath": "/y", "executor": "orca"}}}
	  ]}`))
	want := []string{
		"agentReview.enabled",
		"comments.onReview.de",
		"executors.claude.args",
		"executors.claude.label",
		"maxConcurrent",
		"notifications.timeoutSec",
		"routes.table.work.big.speed",
		"trackers.labels.other", // the tracker is a list item, so the path has no index
		"trackers.projects.P.repos.a.match",
		"trackers.projects.P.typo",
		"trackers.projects.Q.executor",
		"trackers.transitions.onDone",
	}
	if !slices.Equal(got, want) {
		t.Errorf("UnknownKeys\n got %q\nwant %q", got, want)
	}
}

func TestUnknownKeysOfNothing(t *testing.T) {
	for _, in := range []string{``, `[]`, `"x"`, `null`, `{}`, `{"trackers": "x"}`} {
		if got := UnknownKeys([]byte(in)); len(got) != 0 {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// The example is what the README points to: no key the loader ignores, every key it reads shown at least once, and
// it loads.
func TestExampleMatchesTheConfig(t *testing.T) {
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if unknown := UnknownKeys(data); len(unknown) > 0 {
		t.Errorf("mako.config.example.json has keys the loader ignores: %q", unknown)
	}

	w := checkKeys(data)
	known := map[string]bool{}
	allKeys("", reflect.TypeFor[rawConfig](), known)
	delete(known, "language") // an older config's name of agentLanguage: read, never written
	var missing []string
	for k := range known {
		if !w.seen[k] {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("mako.config.example.json does not show the keys %q", missing)
	}

	if _, err := Parse(data, nil); err != nil {
		t.Errorf("the example does not load: %v", err)
	}
}
