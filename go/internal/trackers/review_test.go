package trackers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func TestGitLabReview(t *testing.T) {
	merged, closed := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := r.URL.EscapedPath(); {
		case p == "/api/v4/projects/grp%2Fapp/merge_requests/9" && r.Method == "GET":
			send(map[string]any{"iid": 9, "state": "opened", "web_url": "u", "has_conflicts": false, "detailed_merge_status": "ci_must_pass",
				"head_pipeline": map[string]any{"id": 77, "status": "failed", "web_url": "pipe"}})
		case p == "/api/v4/projects/grp%2Fapp/pipelines/77/jobs":
			send([]map[string]any{{"id": 6, "name": "lint", "stage": "test", "status": "failed", "allow_failure": true},
				{"id": 5, "name": "e2e", "stage": "test", "status": "failed", "web_url": "job5"},
				{"id": 4, "name": "build", "stage": "build", "status": "success"}})
		case p == "/api/v4/projects/grp%2Fapp/merge_requests/9/discussions":
			send([]map[string]any{
				{"notes": []map[string]any{{"id": 1, "body": "Rename it", "resolvable": true, "resolved": false, "author": map[string]any{"username": "ann"},
					"position": map[string]any{"new_path": "a.go", "new_line": 3}}}},
				{"notes": []map[string]any{{"id": 2, "body": "done", "resolvable": true, "resolved": true}}},
				{"notes": []map[string]any{{"id": 3, "body": "added commit", "system": true}}},
			})
		case p == "/api/v4/projects/grp%2Fapp/jobs/5/trace":
			w.Write([]byte("line 1\n\x1b[31mFAIL\x1b[0m checkout\n"))
		case p == "/api/v4/projects/grp%2Fapp/merge_requests/9" && r.Method == "PUT":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			closed = body["state_event"] == "close"
			send(map[string]any{})
		case p == "/api/v4/projects/grp%2Fapp/merge_requests/9/merge" && r.Method == "PUT":
			merged = true
			send(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	url := srv.URL + "/grp/app/-/merge_requests/9"

	c, err := g.CheckReview(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "open" || c.CI != "failed" || len(c.Failed) != 1 || c.Failed[0].Name != "e2e" || c.Mergeable || c.Blocked != "спершу має пройти CI" {
		t.Fatalf("check %+v", c)
	}
	if len(c.Jobs) != 3 || c.Jobs[0].Name != "build" || c.Jobs[2].Stage != "test" || !c.Jobs[2].AllowFailure {
		t.Fatalf("jobs %+v", c.Jobs)
	}
	if len(c.Threads) != 1 || c.Threads[0].Path != "a.go" || c.Threads[0].Author != "ann" || !strings.HasSuffix(c.Threads[0].URL, "#note_1") {
		t.Fatalf("threads %+v", c.Threads)
	}
	if log, _ := g.JobLog(ctx, url, c.Failed[0], 1); log != "FAIL checkout" {
		t.Fatalf("log %q", log)
	}
	if err := g.Merge(ctx, url); err != nil || !merged {
		t.Fatal("not merged", err)
	}
	if err := g.Close(ctx, url); err != nil || !closed {
		t.Fatal("not closed", err)
	}
}

// A running pipeline holds the merge back even where the code host would allow it (no «pipelines must succeed»).
func TestReviewHeldWhileCIRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := r.URL.EscapedPath(); p {
		case "/api/v4/projects/grp%2Fapp/merge_requests/9":
			send(map[string]any{"iid": 9, "state": "opened", "web_url": "u", "detailed_merge_status": "mergeable",
				"head_pipeline": map[string]any{"id": 77, "status": "running", "web_url": "pipe"}})
		case "/repos/o/r/pulls/5":
			send(map[string]any{"number": 5, "state": "open", "mergeable": true, "mergeable_state": "unstable", "head": map[string]any{"sha": "abc"}})
		case "/repos/o/r/commits/abc/check-runs":
			send(map[string]any{"check_runs": []map[string]any{{"id": 1, "name": "test", "status": "in_progress"}}})
		default:
			send([]any{})
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	gl, _ := NewGitLab(&config.Tracker{BaseURL: srv.URL}, "tok")
	if c, err := gl.CheckReview(ctx, srv.URL+"/grp/app/-/merge_requests/9"); err != nil || c.Mergeable || c.Blocked != "CI ще йде" {
		t.Fatalf("gitlab %+v %v", c, err)
	}
	gh, _ := NewGitHub(&config.Tracker{APIURL: srv.URL}, "tok")
	if c, err := gh.CheckReview(ctx, "https://github.com/o/r/pull/5"); err != nil || c.CI != "running" || c.Mergeable || c.Blocked != "CI ще йде" {
		t.Fatalf("github %+v %v", c, err)
	}
}
