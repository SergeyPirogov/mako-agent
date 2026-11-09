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
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

func TestGitHubCreateAndUpload(t *testing.T) {
	var created map[string]any
	var commit map[string]any
	var repo map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/repos/me/app/issues" && r.Method == "POST":
			json.Unmarshal(body, &created)
			w.Write([]byte(`{"number":42,"state":"open","title":"Crash on save","body":"steps","html_url":"https://github.com/me/app/issues/42",
			  "labels":[{"name":"claude-agent"},{"name":"bug"}],"assignees":[{"login":"serhii"}]}`))
		case r.URL.Path == "/repos/me/app":
			w.Write([]byte(`{"name":"app","private":true,"owner":{"login":"me","type":"User"}}`))
		case r.URL.Path == "/repos/me/mako-uploads" && r.Method == "GET":
			w.WriteHeader(404)
		case r.URL.Path == "/user/repos" && r.Method == "POST":
			json.Unmarshal(body, &repo)
			w.WriteHeader(201)
			w.Write([]byte(`{"private":true,"default_branch":"main"}`))
		case strings.HasPrefix(r.URL.Path, "/repos/me/mako-uploads/contents/.mako/uploads/app/") && r.Method == "PUT":
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
	if as, _ := json.Marshal(created["assignees"]); string(as) != `["me"]` {
		t.Errorf("assignees %s", as)
	}
	md, err := g.Upload(ctx, "me/app", "shot 1.png", []byte("PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if repo["name"] != "mako-uploads" || repo["private"] != true || repo["auto_init"] != true {
		t.Errorf("repo %v", repo)
	}
	if commit["branch"] != "main" || commit["content"] != "UE5H" {
		t.Errorf("commit %v", commit)
	}
	if !strings.HasPrefix(md, "![shot 1.png](https://") || !strings.Contains(md, "/me/mako-uploads/blob/main/.mako/uploads/app/") || !strings.HasSuffix(md, "-shot-1.png?raw=true)") {
		t.Errorf("markdown %s", md)
	}
}

// The images repository: one per owner, as private as the project, made only when missing.
func TestGitHubUploadRepo(t *testing.T) {
	cases := []struct {
		name, project, uploads, create string // answers: the project, GET <owner>/mako-uploads ("" = 404), the create
		createPath                     string
		wantPrivate                    bool
		wantLink, wantErr              string
	}{
		{name: "org public project", project: `{"name":"app","private":false,"owner":{"login":"acme","type":"Organization"}}`,
			create: `{"private":false,"default_branch":"main"}`, createPath: "/orgs/acme/repos",
			wantLink: "/acme/mako-uploads/blob/main/.mako/uploads/app/"},
		{name: "existing repo", project: `{"name":"app","private":true,"owner":{"login":"acme","type":"Organization"}}`,
			uploads:  `{"private":true,"default_branch":"trunk"}`,
			wantLink: "/acme/mako-uploads/blob/trunk/.mako/uploads/app/"},
		{name: "public repo private project", project: `{"name":"app","private":true,"owner":{"login":"acme","type":"Organization"}}`,
			uploads: `{"private":false,"default_branch":"main"}`, wantErr: "is public but the project is private"},
		{name: "no rights", project: `{"name":"app","private":true,"owner":{"login":"acme","type":"Organization"}}`,
			create: "403", createPath: "/orgs/acme/repos", wantPrivate: true,
			wantErr: "create acme/mako-uploads manually or give the token permission to create repositories"},
		{name: "someone else's user repo", project: `{"name":"app","private":false,"owner":{"login":"other","type":"User"}}`,
			wantErr: "create other/mako-uploads manually"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var made map[string]any
			var commit map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				owner := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")[0]
				switch {
				case r.URL.Path == "/user":
					w.Write([]byte(`{"login":"me"}`))
				case strings.HasSuffix(r.URL.Path, "/app") && r.Method == "GET":
					w.Write([]byte(c.project))
				case r.URL.Path == "/repos/"+owner+"/mako-uploads" && r.Method == "GET":
					if c.uploads == "" {
						w.WriteHeader(404)
						return
					}
					w.Write([]byte(c.uploads))
				case r.URL.Path == c.createPath && r.Method == "POST":
					json.Unmarshal(body, &made)
					if c.create == "403" {
						w.WriteHeader(403)
						w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
						return
					}
					w.WriteHeader(201)
					w.Write([]byte(c.create))
				case strings.HasPrefix(r.URL.Path, "/repos/"+owner+"/mako-uploads/contents/.mako/uploads/app/") && r.Method == "PUT":
					json.Unmarshal(body, &commit)
					w.WriteHeader(201)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
			md, err := g.Upload(context.Background(), "acme/app", "shot.png", []byte("PNG"))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) || commit != nil {
					t.Errorf("err %v commit %v", err, commit)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.createPath != "" && (made["private"] != c.wantPrivate || made["auto_init"] != true) {
				t.Errorf("made %v", made)
			}
			if c.createPath == "" && made != nil {
				t.Errorf("made %v though the repo exists", made)
			}
			if !strings.Contains(md, c.wantLink) || !strings.HasSuffix(md, "-shot.png?raw=true)") || commit == nil {
				t.Errorf("markdown %s commit %v", md, commit)
			}
		})
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

// The MR's «Closes #N» closes the issue between Mako's look and its PATCH: GitHub answers 422, the issue is closed.
func TestGitHubMoveClosedMeanwhile(t *testing.T) {
	looks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/me/app/issues/7" && r.Method == "GET":
			looks++
			if looks == 1 {
				w.Write([]byte(`{"number":7,"state":"open"}`))
			} else {
				w.Write([]byte(`{"number":7,"state":"closed"}`))
			}
		case r.URL.Path == "/repos/me/app/issues/7" && r.Method == "PATCH":
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"Issue","code":"custom","message":"This issue was closed by another request. Reload it and try again."}]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	if _, err := g.Move(context.Background(), core.Task{ID: "me/app#7", Project: "me/app"}, AtMerged); err != nil {
		t.Errorf("Move: %v", err)
	}
}

// A plain issue («For Mako»): no assignee and no trigger label, so Mako does not take it; who the token is is not
// even asked.
func TestCreatePlain(t *testing.T) {
	var gh, gl map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/repos/me/app/issues" && r.Method == "POST":
			json.Unmarshal(body, &gh)
			w.Write([]byte(`{"number":43,"state":"open","title":"Mako: x","html_url":"https://github.com/me/app/issues/43"}`))
		case r.URL.EscapedPath() == "/api/v4/projects/grp%2Fapp/issues" && r.Method == "POST":
			json.Unmarshal(body, &gl)
			w.Write([]byte(`{"id":901,"iid":6,"state":"opened","title":"Mako: x","web_url":"https://gl/grp/app/-/work_items/6","issue_type":"task"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL, Trigger: "claude-agent"}, "tok")
	if _, err := g.Create(ctx, NewIssue{Project: "me/app", Title: "Mako: x", Description: "facts", Plain: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gh["assignees"]; ok {
		t.Errorf("GitHub: assigned %v", gh)
	}
	if labels, _ := json.Marshal(gh["labels"]); string(labels) != `[]` {
		t.Errorf("GitHub: labels %s", labels)
	}
	l, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL, Trigger: "claude-agent"}, "tok")
	if _, err := l.Create(ctx, NewIssue{Project: "grp/app", Title: "Mako: x", Description: "facts", Plain: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gl["assignee_ids"]; ok || gl["labels"] != "" {
		t.Errorf("GitLab: assigned or labelled %v", gl)
	}
}

// A picture an issue holds ("/uploads/<secret>/<file>") is read through the API with the token; a path that is not
// an upload is never sent to GitLab.
func TestGitLabImage(t *testing.T) {
	const secret = "35e4b8215c97b932f4083d09e557b2d0"
	png := "\x89PNG\r\n\x1a\nbinary\x00\xff"
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.EscapedPath())
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			t.Errorf("token %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		if r.URL.EscapedPath() != "/api/v4/projects/ecom%2Ftests%2Fmoq/uploads/"+secret+"/my%20shot.png" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(png))
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	got, err := g.Image(ctx, "ecom/tests/moq", "/uploads/"+secret+"/my shot.png")
	if err != nil || string(got) != png {
		t.Fatalf("image %q %v", got, err)
	}
	for _, bad := range []string{"/uploads/../../user", "/uploads/" + secret + "/a/b.png", "https://evil.example/x.png", "/api/v4/user", "/uploads/zz/a.png"} {
		if _, err := g.Image(ctx, "ecom/tests/moq", bad); err == nil {
			t.Errorf("%q was fetched", bad)
		}
	}
	if _, err := g.Image(ctx, "ecom/tests/moq", "/uploads/"+secret+"/gone.png"); err == nil {
		t.Error("a missing picture is no error")
	}
	if len(asked) != 2 {
		t.Errorf("asked GitLab %v", asked)
	}
}
