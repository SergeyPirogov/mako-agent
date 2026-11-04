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

func TestGitHubWatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/repos/me/app":
			send(map[string]any{"default_branch": "main"})
		case "/repos/me/app/commits/main":
			send(map[string]any{"sha": "abc", "html_url": "https://github.com/me/app/commit/abc"})
		case "/repos/me/app/commits/abc/check-runs":
			send(map[string]any{"check_runs": []map[string]any{
				{"id": 11, "name": "test", "status": "completed", "conclusion": "failure", "html_url": "run11"},
				{"id": 12, "name": "lint", "status": "completed", "conclusion": "success"},
			}})
		case "/repos/me/app/actions/jobs/11/logs":
			w.Write([]byte("setup\n--- FAIL: TestSave\nexit 1\n"))
		case "/repos/me/app/dependabot/alerts":
			if r.URL.Query().Get("state") != "open" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			send([]map[string]any{{"number": 3, "html_url": "alert3",
				"security_advisory":      map[string]any{"summary": "Prototype pollution", "severity": "high"},
				"security_vulnerability": map[string]any{"severity": "critical", "package": map[string]any{"name": "lodash"}}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	ctx := context.Background()
	b, err := g.DefaultCI(ctx, "me/app")
	if err != nil {
		t.Fatal(err)
	}
	if b.Branch != "main" || b.SHA != "abc" || b.CI != "failed" || b.URL != "https://github.com/me/app/commit/abc/checks" || len(b.Failed) != 1 || b.Failed[0].Name != "test" {
		t.Fatalf("ci %+v", b)
	}
	if log, err := g.BranchJobLog(ctx, "me/app", b.Failed[0], 2); err != nil || log != "--- FAIL: TestSave\nexit 1" {
		t.Fatalf("log %q %v", log, err)
	}
	vs, err := g.Vulnerabilities(ctx, "me/app")
	if err != nil || len(vs) != 1 || vs[0] != (Vulnerability{ID: "3", Package: "lodash", Severity: "critical", Summary: "Prototype pollution", URL: "alert3"}) {
		t.Fatalf("vulnerabilities %+v %v", vs, err)
	}
}

func TestGitLabWatch(t *testing.T) {
	ultimate := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := r.URL.EscapedPath(); {
		case p == "/api/v4/projects/grp%2Fapp":
			send(map[string]any{"default_branch": "develop"})
		case p == "/api/v4/projects/grp%2Fapp/pipelines":
			if r.URL.Query().Get("ref") != "develop" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			send([]map[string]any{{"id": 77, "sha": "def", "status": "failed", "web_url": "pipe77"}})
		case p == "/api/v4/projects/grp%2Fapp/pipelines/77/jobs":
			send([]map[string]any{{"id": 6, "name": "lint", "status": "failed", "allow_failure": true},
				{"id": 5, "name": "unit", "status": "failed", "web_url": "job5"}})
		case p == "/api/v4/projects/grp%2Fapp/jobs/5/trace":
			w.Write([]byte("Error: cannot find module\n"))
		case p == "/api/graphql":
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if !strings.Contains(body.Query, "vulnerabilities") || body.Variables["p"] != "grp/app" {
				t.Errorf("graphql %+v", body)
			}
			if !ultimate {
				send(map[string]any{"data": map[string]any{"project": map[string]any{"vulnerabilities": nil}}})
				return
			}
			send(map[string]any{"data": map[string]any{"project": map[string]any{"vulnerabilities": map[string]any{"nodes": []map[string]any{
				{"id": "gid://gitlab/Vulnerability/9", "title": "CVE-2026-1 in express", "severity": "HIGH", "webUrl": "v9"}}}}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	b, err := g.DefaultCI(ctx, "grp/app")
	if err != nil {
		t.Fatal(err)
	}
	if b.Branch != "develop" || b.SHA != "def" || b.CI != "failed" || b.URL != "pipe77" || len(b.Failed) != 1 || b.Failed[0].Name != "unit" {
		t.Fatalf("ci %+v", b)
	}
	if log, err := g.BranchJobLog(ctx, "grp/app", b.Failed[0], 5); err != nil || log != "Error: cannot find module" {
		t.Fatalf("log %q %v", log, err)
	}
	vs, err := g.Vulnerabilities(ctx, "grp/app")
	if err != nil || len(vs) != 1 || vs[0].ID != "gid://gitlab/Vulnerability/9" || vs[0].Severity != "high" || vs[0].Summary != "CVE-2026-1 in express" {
		t.Fatalf("vulnerabilities %+v %v", vs, err)
	}
	ultimate = false
	if _, err := g.Vulnerabilities(ctx, "grp/app"); err == nil || !strings.Contains(err.Error(), "Ultimate") {
		t.Fatalf("without Ultimate: %v", err)
	}
}
