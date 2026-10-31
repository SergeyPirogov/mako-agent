package trackers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestJira(t *testing.T) {
	var transitioned []string
	var comment map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/rest/api/3/search/jql":
			var q map[string]any
			json.Unmarshal(body, &q)
			if !strings.Contains(q["jql"].(string), "currentUser()") {
				t.Errorf("jql %v", q["jql"])
			}
			w.Write([]byte(`{"issues":[{"key":"PAY-7","fields":{"summary":"Refund fails","issuetype":{"name":"Bug"},"priority":{"name":"High"},
			  "project":{"key":"PAY"},"labels":["backend"],"assignee":{"accountId":"acc-1"},
			  "description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Steps:"}]},
			    {"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"open refund"}]}]}]}]},
			  "comment":{"comments":[{"author":{"displayName":"Ann"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Since Monday"}]}]}}]}}}]}`))
		case r.URL.Path == "/rest/api/3/issue/PAY-7/transitions" && r.Method == "GET":
			w.Write([]byte(`{"transitions":[{"id":"21","name":"Start","to":{"name":"In Progress"}},{"id":"31","name":"Done","to":{"name":"Done"}}]}`))
		case r.URL.Path == "/rest/api/3/issue/PAY-7/transitions":
			var q struct{ Transition struct{ ID string } }
			json.Unmarshal(body, &q)
			transitioned = append(transitioned, q.Transition.ID)
			w.WriteHeader(204)
		case r.URL.Path == "/rest/api/3/issue/PAY-7/comment":
			json.Unmarshal(body, &comment)
			w.Write([]byte(`{}`))
		case r.URL.Path == "/rest/api/3/issue/PAY-7":
			w.Write([]byte(`{"key":"PAY-7","fields":{"status":{"statusCategory":{"key":"done"}}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	start := "In Progress"
	j, err := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL, Transitions: config.Transitions{OnStart: &start}}, "me@x", "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tasks, err := j.FetchMine(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%v %v", err, tasks)
	}
	task := tasks[0]
	if task.Meta != "Type: Bug | Priority: High" || task.Description != "Steps:\n\n- open refund" || task.Comments[0].Body != "Since Monday" {
		t.Errorf("task %+v", task)
	}
	if st, err := j.Move(ctx, task, AtStart); err != nil || st != "In Progress" {
		t.Errorf("start: %q %v", st, err)
	}
	if _, err := j.Move(ctx, task, AtMerged); err != nil {
		t.Error(err)
	}
	if strings.Join(transitioned, ",") != "21,31" {
		t.Errorf("transitions %v", transitioned)
	}
	j.Comment(ctx, task, "line 1\nline 2\n\npara")
	if comment["body"].(map[string]any)["type"] != "doc" {
		t.Errorf("comment %v", comment)
	}
	if st, _ := j.IssueState(ctx, task); st != "closed" {
		t.Errorf("state %s", st)
	}
}

func TestGitHub(t *testing.T) {
	var closed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/issues":
			w.Write([]byte(`[{"number":3,"state":"open","title":"Add dark mode","html_url":"https://github.com/acme/web/issues/3","labels":[{"name":"enhancement"}],
			  "assignees":[{"login":"serg"}],"repository":{"full_name":"acme/web"}},
			  {"number":4,"state":"open","title":"A PR","pull_request":{},"repository":{"full_name":"acme/web"}}]`))
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"serg"}`))
		case r.URL.Path == "/repos/acme/web/pulls":
			if r.URL.Query().Get("head") != "acme:feature/3-add-dark-mode" {
				t.Errorf("head %s", r.URL.Query().Get("head"))
			}
			w.Write([]byte(`[{"number":12,"state":"closed","merged_at":"2026-10-01T10:00:00Z","html_url":"https://github.com/acme/web/pull/12"}]`))
		case r.URL.Path == "/repos/acme/web/issues/3" && r.Method == "PATCH":
			closed = true
			w.Write([]byte(`{}`))
		case r.URL.Path == "/repos/acme/web/issues/3":
			w.Write([]byte(`{"number":3,"state":"open"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "github", APIURL: srv.URL, AllProjects: true, TriggerOff: true}, "tok")
	ctx := context.Background()
	tasks, err := g.FetchMine(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "acme/web#3" || tasks[0].ClosingRef != "Closes acme/web#3" {
		t.Fatalf("%v %+v", err, tasks)
	}
	review, err := g.FindReview(ctx, "acme/web", "feature/3-add-dark-mode")
	if err != nil || review.State != "merged" || review.Number != 12 {
		t.Fatalf("%v %+v", err, review)
	}
	g.Move(ctx, core.Task{ID: "acme/web#3", Project: "acme/web"}, AtMerged)
	if !closed {
		t.Error("issue not closed after merge")
	}
}

func TestADF(t *testing.T) {
	raw := json.RawMessage(`{"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Why"}]},
	  {"type":"paragraph","content":[{"type":"text","text":"see "},{"type":"inlineCard","attrs":{"url":"https://x"}},{"type":"hardBreak"},{"type":"mention","attrs":{"text":"@Ann"}}]},
	  {"type":"codeBlock","content":[{"type":"text","text":"go test"}]}]}`)
	want := "## Why\n\nsee https://x\n@Ann\n\n```\ngo test\n```\n\n"
	if got := adfToText(raw); got != want {
		t.Errorf("got %q", got)
	}
}

// FetchMine reads every page of the list, not only the first perPage issues.
func TestFetchMinePages(t *testing.T) {
	const total = perPage + 50
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Write([]byte(`{"login":"serg"}`))
			return
		}
		if r.URL.Path != "/issues" {
			w.WriteHeader(404)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		pages = append(pages, r.URL.Query().Get("page"))
		items := []string{}
		for n := (page-1)*size + 1; n <= min(page*size, total); n++ {
			items = append(items, fmt.Sprintf(`{"number":%d,"state":"open","title":"T%d","repository":{"full_name":"acme/web"}}`, n, n))
		}
		w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "github", APIURL: srv.URL, AllProjects: true, TriggerOff: true}, "tok")
	tasks, err := g.FetchMine(context.Background())
	if err != nil || len(tasks) != total || tasks[total-1].ID != fmt.Sprintf("acme/web#%d", total) {
		t.Fatalf("%v: %d tasks", err, len(tasks))
	}
	if strings.Join(pages, ",") != "1,2" {
		t.Errorf("pages %v", pages)
	}
}
