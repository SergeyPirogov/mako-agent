package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// The pages get their words in English, whatever the agent language.
func TestI18n(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), AgentLanguage: "uk"}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: t.TempDir()}).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/i18n?lang=uk")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var words struct {
		Lang    string            `json:"lang"`
		Strings map[string]string `json:"strings"`
	}
	json.NewDecoder(res.Body).Decode(&words)
	if words.Lang != "en" || words.Strings["s.save"] != "Save" {
		t.Fatalf("en: %s %q", words.Lang, words.Strings["s.save"])
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
