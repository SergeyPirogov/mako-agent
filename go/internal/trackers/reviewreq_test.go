package trackers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func TestGitLabToReview(t *testing.T) {
	diffCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := r.URL.EscapedPath(); p {
		case "/api/v4/user":
			send(map[string]any{"id": 7, "username": "serg"})
		case "/api/v4/merge_requests":
			q := r.URL.Query()
			if q.Get("reviewer_username") != "serg" || q.Get("state") != "opened" || q.Get("scope") != "all" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			send([]map[string]any{
				{"iid": 4, "title": "Add cache", "description": "Why", "web_url": "http://h/grp/app/-/merge_requests/4", "sha": "abc",
					"author": map[string]any{"username": "ann"}, "references": map[string]any{"full": "grp/app!4"}},
				{"iid": 5, "title": "Done by me", "web_url": "http://h/grp/app/-/merge_requests/5", "sha": "def",
					"author": map[string]any{"username": "bob"}, "references": map[string]any{"full": "grp/app!5"}},
			})
		case "/api/v4/projects/grp%2Fapp/merge_requests/4":
			send(map[string]any{"iid": 4, "head_pipeline": map[string]any{"id": 1, "status": "running"}})
		case "/api/v4/projects/grp%2Fapp/merge_requests/5":
			send(map[string]any{"iid": 5})
		case "/api/v4/projects/grp%2Fapp/merge_requests/4/approvals":
			send(map[string]any{"approved_by": []any{}})
		case "/api/v4/projects/grp%2Fapp/merge_requests/5/approvals":
			send(map[string]any{"approved_by": []any{map[string]any{"user": map[string]any{"username": "serg"}}}})
		case "/api/v4/projects/grp%2Fapp/merge_requests/4/diffs", "/api/v4/projects/grp%2Fapp/merge_requests/5/diffs":
			diffCalls++
			send([]map[string]any{
				{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1,2 +1,3 @@\n a\n-b\n+c\n+d\n"},
				{"old_path": "old.go", "new_path": "new.go", "renamed_file": true, "diff": ""},
				{"old_path": "x.go", "new_path": "x.go", "new_file": true, "diff": "@@ -0,0 +1 @@\n+x\n"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	rs, err := g.ToReview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2, got %+v", rs)
	}
	a := rs[0]
	if a.ID != "http://h/grp/app/-/merge_requests/4" || a.Project != "grp/app" || a.Ref != "!4" || a.Author != "ann" || a.CI != "running" ||
		a.HeadSHA != "abc" || a.Additions != 3 || a.Deletions != 1 || a.Approved || a.TrackerID != "gl" {
		t.Fatalf("mr %+v", a)
	}
	if !rs[1].Approved {
		t.Fatalf("approved by me: %+v", rs[1])
	}
	// The size is read once per head commit.
	if _, err := g.ToReview(ctx); err != nil || diffCalls != 2 {
		t.Fatalf("diffs read %d times, %v", diffCalls, err)
	}
	files, err := g.ReviewFiles(ctx, srv.URL+"/grp/app/-/merge_requests/4")
	if err != nil || len(files) != 3 {
		t.Fatalf("files %+v %v", files, err)
	}
	if files[0].Status != "M" || files[0].Additions != 2 || files[1].Status != "R" || files[1].OldPath != "old.go" || files[2].Status != "A" {
		t.Fatalf("files %+v", files)
	}
}

func TestGitHubToReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		item := func(n int, author string) map[string]any {
			return map[string]any{"number": n, "title": "PR", "body": "b", "html_url": "https://github.com/o/r/pull/" + string(rune('0'+n)),
				"user": map[string]any{"login": author}}
		}
		switch r.URL.Path {
		case "/user":
			send(map[string]any{"login": "serg"})
		case "/search/issues":
			switch r.URL.Query().Get("q") {
			case "is:pr is:open review-requested:@me":
				send(map[string]any{"items": []any{item(1, "ann")}})
			case "is:pr is:open reviewed-by:@me":
				// 1 again (both searches find it), 2 reviewed and approved, 3 with changes asked, 4 my own
				send(map[string]any{"items": []any{item(1, "ann"), item(2, "bob"), item(3, "bob"), item(4, "serg")}})
			default:
				t.Errorf("q %q", r.URL.Query().Get("q"))
			}
		case "/repos/o/r/pulls/1", "/repos/o/r/pulls/2", "/repos/o/r/pulls/3":
			send(map[string]any{"head": map[string]any{"sha": "s" + r.URL.Path[len(r.URL.Path)-1:]}, "additions": 10, "deletions": 2})
		case "/repos/o/r/commits/s1/check-runs":
			send(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "test", "status": "completed", "conclusion": "failure"}}})
		case "/repos/o/r/commits/s2/check-runs", "/repos/o/r/commits/s3/check-runs":
			send(map[string]any{"check_runs": []any{}})
		case "/repos/o/r/pulls/2/reviews":
			send([]any{map[string]any{"state": "CHANGES_REQUESTED", "user": map[string]any{"login": "serg"}},
				map[string]any{"state": "APPROVED", "user": map[string]any{"login": "serg"}},
				map[string]any{"state": "COMMENTED", "user": map[string]any{"login": "serg"}}})
		case "/repos/o/r/pulls/3/reviews":
			send([]any{map[string]any{"state": "CHANGES_REQUESTED", "user": map[string]any{"login": "serg"}}})
		case "/repos/o/r/pulls/1/files":
			send([]any{map[string]any{"filename": "a.go", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@\n-a\n+b"},
				map[string]any{"filename": "b.go", "previous_filename": "c.go", "status": "renamed"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	ctx := context.Background()
	rs, err := g.ToReview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("want 3 without duplicates or my own, got %+v", rs)
	}
	if a := rs[0]; a.Ref != "#1" || a.Project != "o/r" || a.CI != "failed" || a.Additions != 10 || a.Deletions != 2 || a.HeadSHA != "s1" || a.Approved {
		t.Fatalf("pr 1 %+v", a)
	}
	if !rs[1].Approved || rs[2].Approved {
		t.Fatalf("approved: 2 %v, 3 %v", rs[1].Approved, rs[2].Approved)
	}
	files, err := g.ReviewFiles(ctx, "https://github.com/o/r/pull/1")
	if err != nil || len(files) != 2 || files[0].Diff == "" || files[1].Status != "R" || files[1].OldPath != "c.go" {
		t.Fatalf("files %+v %v", files, err)
	}
}
