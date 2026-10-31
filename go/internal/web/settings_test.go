package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
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
	if len(got) != 1 || !strings.Contains(got[0], "«SergeyPirgov» немає") {
		t.Errorf("unknown: %q", got)
	}
}
