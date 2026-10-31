package trackers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

func TestGitHubCreateAndUpload(t *testing.T) {
	var created map[string]any
	var commit map[string]any
	var branch map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"serhii"}`))
		case r.URL.Path == "/repos/me/app/issues" && r.Method == "POST":
			json.Unmarshal(body, &created)
			w.Write([]byte(`{"number":42,"state":"open","title":"Crash on save","body":"steps","html_url":"https://github.com/me/app/issues/42",
			  "labels":[{"name":"claude-agent"},{"name":"bug"}],"assignees":[{"login":"serhii"}]}`))
		case r.URL.Path == "/repos/me/app/git/ref/heads/mako-uploads":
			w.WriteHeader(404)
		case r.URL.Path == "/repos/me/app":
			w.Write([]byte(`{"default_branch":"main"}`))
		case r.URL.Path == "/repos/me/app/git/ref/heads/main":
			w.Write([]byte(`{"object":{"sha":"abc"}}`))
		case r.URL.Path == "/repos/me/app/git/refs" && r.Method == "POST":
			json.Unmarshal(body, &branch)
			w.WriteHeader(201)
		case strings.HasPrefix(r.URL.Path, "/repos/me/app/contents/.mako/uploads/") && r.Method == "PUT":
			json.Unmarshal(body, &commit)
			w.WriteHeader(201)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL, Trigger: "claude-agent"}, "tok")
	ctx := context.Background()
	task, err := g.Create(ctx, NewIssue{Project: "me/app", Title: "Crash on save", Description: "steps", Bug: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "me/app#42" || gitx.TaskType(task) != "fix" {
		t.Errorf("task %+v", task)
	}
	if created["title"] != "Crash on save" || created["body"] != "steps" {
		t.Errorf("created %v", created)
	}
	if labels, _ := json.Marshal(created["labels"]); string(labels) != `["claude-agent","bug"]` {
		t.Errorf("labels %s", labels)
	}
	if as, _ := json.Marshal(created["assignees"]); string(as) != `["serhii"]` {
		t.Errorf("assignees %s", as)
	}
	md, err := g.Upload(ctx, "me/app", "shot 1.png", []byte("PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if branch["ref"] != "refs/heads/mako-uploads" || branch["sha"] != "abc" || commit["branch"] != "mako-uploads" || commit["content"] != "UE5H" {
		t.Errorf("branch %v commit %v", branch, commit)
	}
	if !strings.HasPrefix(md, "![shot 1.png](https://") || !strings.Contains(md, "/me/app/blob/mako-uploads/.mako/uploads/") || !strings.HasSuffix(md, "-shot-1.png?raw=true)") {
		t.Errorf("markdown %s", md)
	}
}

func TestGitLabCreateAndUpload(t *testing.T) {
	var created map[string]any
	var uploaded string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/api/v4/user":
			w.Write([]byte(`{"id":7,"username":"serhii"}`))
		case r.URL.EscapedPath() == "/api/v4/projects/grp%2Fapp/issues" && r.Method == "POST":
			json.Unmarshal(body, &created)
			w.Write([]byte(`{"id":900,"iid":5,"state":"opened","title":"Add export","web_url":"https://gl/grp/app/-/work_items/5","issue_type":"task","labels":["claude-agent"]}`))
		case r.URL.EscapedPath() == "/api/v4/projects/grp%2Fapp/uploads" && r.Method == "POST":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				t.Errorf("content type %s", r.Header.Get("Content-Type"))
			}
			uploaded = string(body)
			w.Write([]byte(`{"markdown":"![shot.png](/uploads/x/shot.png)"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL, Trigger: "claude-agent"}, "tok")
	ctx := context.Background()
	task, err := g.Create(ctx, NewIssue{Project: "grp/app", Title: "Add export"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "grp/app#5" || gitx.TaskType(task) != "feature" {
		t.Errorf("task %+v", task)
	}
	if created["issue_type"] != "task" || created["labels"] != "claude-agent" || created["assignee_ids"].([]any)[0] != 7.0 {
		t.Errorf("created %v", created)
	}
	if _, err := g.Create(ctx, NewIssue{Project: "grp/app", Title: "Broken", Bug: true}); err != nil || created["issue_type"] != "issue" {
		t.Errorf("bug: %v %v", err, created)
	}
	md, err := g.Upload(ctx, "grp/app", "shot.png", []byte("PNG-DATA"))
	if err != nil || md != "![shot.png](/uploads/x/shot.png)" || !strings.Contains(uploaded, `filename="shot.png"`) || !strings.Contains(uploaded, "PNG-DATA") {
		t.Errorf("upload %q %v %q", md, err, uploaded)
	}
}
