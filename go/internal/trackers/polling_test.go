package trackers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// The statuses are kept per project: another project does not get the first one's names, and a failed read is
// not kept as «no statuses» until a restart.
func TestGitLabGuessPerProject(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			http.NotFound(w, r)
			return
		}
		var q struct {
			Variables struct{ Path string } `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		mu.Lock()
		asked[q.Variables.Path]++
		n := asked[q.Variables.Path]
		mu.Unlock()
		statuses := map[string][]string{"grp/a": {"To do", "In progress", "Done"}, "grp/b": {"Backlog", "Doing", "Finished"}}[q.Variables.Path]
		if q.Variables.Path == "grp/b" && n == 1 {
			w.WriteHeader(500) // a POST: not tried again by call
			return
		}
		names := []any{}
		for _, s := range statuses {
			names = append(names, map[string]any{"name": s})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"project": map[string]any{"workItemTypes": map[string]any{"nodes": []any{
			map[string]any{"name": "Issue", "widgetDefinitions": []any{map[string]any{"allowedStatuses": names}}}}}}}})
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gitlab", BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	progress := regexp.MustCompile(`(?i)progress|doing`)
	guess := func(project string) string {
		return g.guess(ctx, core.Task{Project: project}, progress, "in progress")
	}
	if got := guess("grp/b"); got != "" {
		t.Fatalf("b, its read failed: %q", got)
	}
	if got := guess("grp/a"); got != "In progress" {
		t.Fatalf("a: %q", got)
	}
	if got := guess("grp/b"); got != "Doing" {
		t.Fatalf("b, read again: %q", got)
	}
	guess("grp/a")
	guess("grp/b")
	if asked["grp/a"] != 1 || asked["grp/b"] != 2 {
		t.Fatalf("asked %v", asked)
	}
}

// FetchMine reads every page of an issue's comments, and reads them again only when the issue changed.
func TestGitHubCommentsCached(t *testing.T) {
	const total = perPage + 20
	var mu sync.Mutex
	updated, commentReads := "2026-01-01T00:00:00Z", 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/repos/o/r/issues":
			fmt.Fprintf(w, `[{"number":1,"state":"open","title":"T","comments":%d,"updated_at":%q}]`, total, updated)
		case "/repos/o/r/issues/1/comments":
			commentReads++
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			items := []any{}
			for n := (page-1)*perPage + 1; n <= min(page*perPage, total); n++ {
				items = append(items, map[string]any{"body": fmt.Sprintf("c%d", n), "user": map[string]any{"login": "ann"}})
			}
			json.NewEncoder(w).Encode(items)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL, Projects: map[string]*config.Project{"o/r": {}}, TriggerOff: true}, "tok")
	ctx := context.Background()
	fetch := func() core.Task {
		t.Helper()
		tasks, err := g.FetchMine(ctx)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("%v %+v", err, tasks)
		}
		return tasks[0]
	}
	if task := fetch(); len(task.Comments) != total || task.Comments[total-1].Body != fmt.Sprintf("c%d", total) || commentReads != 2 {
		t.Fatalf("first: %d comments, %d reads", len(task.Comments), commentReads)
	}
	if task := fetch(); len(task.Comments) != total || commentReads != 2 {
		t.Fatalf("unchanged: %d comments, %d reads", len(task.Comments), commentReads)
	}
	mu.Lock()
	updated = "2026-01-02T00:00:00Z"
	mu.Unlock()
	if task := fetch(); len(task.Comments) != total || commentReads != 4 {
		t.Fatalf("updated: %d comments, %d reads", len(task.Comments), commentReads)
	}
}

// The commit statuses (Jenkins, Travis) count as CI next to the check runs; CI run again on the same head (the
// PR's updated_at moves) is read anew though it had settled.
func TestGitHubCommitStatusAndRerun(t *testing.T) {
	var mu sync.Mutex
	updated, state := "u1", "failure"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/user":
			send(map[string]any{"login": "serg"})
		case "/search/issues":
			items := []any{}
			if r.URL.Query().Get("q") == "is:pr is:open review-requested:@me" {
				items = append(items, map[string]any{"number": 1, "html_url": "https://github.com/o/r/pull/1", "user": map[string]any{"login": "ann"}, "updated_at": updated})
			}
			send(map[string]any{"items": items})
		case "/repos/o/r/pulls/1":
			send(map[string]any{"head": map[string]any{"sha": "s1"}})
		case "/repos/o/r/pulls/1/reviews":
			send([]any{})
		case "/repos/o/r/commits/s1/check-runs":
			send(map[string]any{"total_count": 0, "check_runs": []any{}})
		case "/repos/o/r/commits/s1/status":
			send(map[string]any{"state": state, "statuses": []any{map[string]any{"context": "jenkins", "state": state, "target_url": "https://ci/1"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	ctx := context.Background()
	ci := func() string {
		t.Helper()
		rs, err := g.ToReview(ctx)
		if err != nil || len(rs) != 1 {
			t.Fatalf("%v %+v", err, rs)
		}
		return rs[0].CI
	}
	if got := ci(); got != "failed" {
		t.Fatalf("jenkins failed: %q", got)
	}
	mu.Lock()
	state = "success"
	mu.Unlock()
	if got := ci(); got != "failed" {
		t.Fatalf("settled and not updated: %q", got)
	}
	mu.Lock()
	updated = "u2"
	mu.Unlock()
	if got := ci(); got != "success" {
		t.Fatalf("run again: %q", got)
	}
	ci2, jobs := g.checkRuns(ctx, "o/r", "s1")
	if ci2 != "success" || len(jobs) != 1 || jobs[0].Name != "jenkins" || jobs[0].URL != "https://ci/1" || jobs[0].ID != 0 {
		t.Fatalf("jobs %q %+v", ci2, jobs)
	}
	// A status has no Actions log: none is asked for.
	if log, err := g.JobLog(ctx, "https://github.com/o/r/pull/1", jobs[0], 10); err != nil || log != "" {
		t.Fatalf("log %q %v", log, err)
	}
}

// GitLab: an MR updated since the last poll (its pipeline run again on the same head) has its CI read anew.
func TestGitLabReviewCIRerun(t *testing.T) {
	var mu sync.Mutex
	updated, status, mrReads := "u1", "failed", 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch r.URL.EscapedPath() {
		case "/api/v4/user":
			send(map[string]any{"id": 7, "username": "serg"})
		case "/api/v4/merge_requests":
			send([]any{map[string]any{"iid": 4, "web_url": "http://h/grp/app/-/merge_requests/4", "sha": "abc", "updated_at": updated,
				"author": map[string]any{"username": "ann"}, "references": map[string]any{"full": "grp/app!4"}}})
		case "/api/v4/projects/grp%2Fapp/merge_requests/4":
			mrReads++
			send(map[string]any{"iid": 4, "head_pipeline": map[string]any{"id": 1, "status": status}})
		default:
			send([]any{})
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gitlab", BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	ci := func() string {
		t.Helper()
		rs, err := g.ToReview(ctx)
		if err != nil || len(rs) != 1 {
			t.Fatalf("%v %+v", err, rs)
		}
		return rs[0].CI
	}
	ci()
	mu.Lock()
	status = "success"
	mu.Unlock()
	if got := ci(); got != "failed" || mrReads != 1 {
		t.Fatalf("settled: %q, %d reads", got, mrReads)
	}
	mu.Lock()
	updated = "u2"
	mu.Unlock()
	if got := ci(); got != "success" || mrReads != 2 {
		t.Fatalf("run again: %q, %d reads", got, mrReads)
	}
}

// Jira's search is read page by page by nextPageToken, not cut at its first page.
func TestJiraSearchPages(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/3/myself" {
			w.Write([]byte(`{"accountId":"acc-1"}`))
			return
		}
		if r.URL.Path != "/rest/api/3/search/jql" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var q struct {
			NextPageToken string `json:"nextPageToken"`
			MaxResults    int    `json:"maxResults"`
		}
		json.Unmarshal(body, &q)
		tokens = append(tokens, q.NextPageToken)
		if q.MaxResults > perPage {
			t.Errorf("maxResults %d", q.MaxResults)
		}
		issue := func(key string) map[string]any {
			return map[string]any{"key": key, "fields": map[string]any{"summary": key, "project": map[string]any{"key": "PAY"}}}
		}
		switch q.NextPageToken {
		case "":
			json.NewEncoder(w).Encode(map[string]any{"issues": []any{issue("PAY-1"), issue("PAY-2")}, "nextPageToken": "t2"})
		case "t2":
			json.NewEncoder(w).Encode(map[string]any{"issues": []any{issue("PAY-3")}, "isLast": true})
		}
	}))
	defer srv.Close()
	j, _ := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	tasks, err := j.FetchMine(context.Background())
	if err != nil || len(tasks) != 3 || tasks[2].ID != "PAY-3" || len(tokens) != 2 || tokens[1] != "t2" {
		t.Fatalf("%v: %d tasks, tokens %q", err, len(tasks), tokens)
	}
	tokens = nil
	if one, err := j.search(context.Background(), "key = PAY-1", 1); err != nil || len(one) != 1 || len(tokens) != 1 {
		t.Fatalf("max 1: %v %d, tokens %q", err, len(one), tokens)
	}
}

// A token that may read the check runs but not the commit statuses (403) still gets the CI of the check runs.
func TestGitHubCheckRunsWithoutStatusAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/s1/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []any{
				map[string]any{"id": 5, "name": "test", "status": "completed", "conclusion": "failure", "html_url": "https://gh/5"}}})
		case "/repos/o/r/commits/s1/status":
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	ci, jobs := g.checkRuns(context.Background(), "o/r", "s1")
	if ci != "failed" || len(jobs) != 1 || jobs[0].ID != 5 || jobs[0].Status != "failed" {
		t.Fatalf("%q %+v", ci, jobs)
	}
	// The check runs themselves unreadable: no CI, as before.
	if ci, jobs := g.checkRuns(context.Background(), "o/r", "s2"); ci != "" || jobs != nil {
		t.Fatalf("no check runs: %q %+v", ci, jobs)
	}
}
