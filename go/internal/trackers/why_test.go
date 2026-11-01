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

// «Чому немає задачі?»: each of the search's filters is checked for one issue by its link.
func TestGitHubWhy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Write([]byte(`{"login":"serg"}`))
		case "/repos/acme/web/issues/1":
			w.Write([]byte(`{"number":1,"state":"open","title":"Fine","labels":[{"name":"claude-agent"}],"assignees":[{"login":"serg"}]}`))
		case "/repos/acme/web/issues/2":
			w.Write([]byte(`{"number":2,"state":"closed","title":"No label, Ann's","labels":[],"assignees":[{"login":"ann"}]}`))
		case "/repos/acme/web/issues/3":
			w.Write([]byte(`{"number":3,"state":"open","title":"A PR","pull_request":{}}`))
		case "/repos/other/app/issues/4":
			w.Write([]byte(`{"number":4,"state":"open","title":"Elsewhere","labels":[{"name":"claude-agent"}],"assignees":[{"login":"serg"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.Index(host, ":")]
	g, _ := NewGitHub(&config.Tracker{ID: "github", APIURL: srv.URL, Trigger: "claude-agent", Assignee: "me", SkipOthersTasks: true,
		Projects: map[string]*config.Project{"acme/web": {}}}, "tok")
	ctx := context.Background()
	failed := func(link string) []string {
		t.Helper()
		task, checks, err := g.Why(ctx, link)
		if err != nil || task == nil {
			t.Fatalf("%s: %v %v", link, task, err)
		}
		out := []string{}
		for _, c := range checks {
			if !c.OK {
				out = append(out, c.Text)
			}
		}
		return out
	}
	if f := failed("https://" + host + "/acme/web/issues/1"); len(f) != 0 {
		t.Errorf("a fitting issue: %v", f)
	}
	f := strings.Join(failed(host+"/acme/web/issues/2"), "\n")
	for _, want := range []string{"is closed", "label \"claude-agent\"", "assigned to ann", "assigned to others"} {
		if !strings.Contains(f, want) {
			t.Errorf("no %q in %s", want, f)
		}
	}
	if f := failed("https://" + host + "/acme/web/pull/3"); len(f) != 1 || !strings.Contains(f[0], "pull request") {
		t.Errorf("a PR: %v", f)
	}
	if f := failed("https://" + host + "/other/app/issues/4"); len(f) != 1 || !strings.Contains(f[0], "Project other/app is not") {
		t.Errorf("a project not in the settings: %v", f)
	}
	if task, _, err := g.Why(ctx, "https://gitlab.com/acme/web/-/issues/1"); task != nil || err != nil {
		t.Errorf("another host's link: %v %v", task, err)
	}
	if _, _, err := g.Why(ctx, "https://"+host+"/acme/web/issues/9"); err == nil {
		t.Error("a missing issue gave no error")
	}
}

func TestJiraWhy(t *testing.T) {
	var jqls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/search/jql":
			var q struct{ JQL string }
			json.NewDecoder(r.Body).Decode(&q)
			jqls = append(jqls, q.JQL)
			// PAY-8 is someone else's: the configured JQL does not find it.
			if strings.Contains(q.JQL, "PAY-8") && strings.Contains(q.JQL, "currentUser()") {
				w.Write([]byte(`{"issues":[]}`))
				return
			}
			key := "PAY-7"
			if strings.Contains(q.JQL, "PAY-8") {
				key = "PAY-8"
			}
			w.Write([]byte(`{"issues":[{"key":"` + key + `","fields":{"summary":"Refund","project":{"key":"PAY"},"issuetype":{"name":"Bug"}}}]}`))
		case "/rest/api/3/issue/PAY-7", "/rest/api/3/issue/PAY-8":
			w.Write([]byte(`{"fields":{"status":{"statusCategory":{"key":"indeterminate"}}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	j, _ := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	ctx := context.Background()
	task, checks, err := j.Why(ctx, srv.URL+"/browse/PAY-7")
	if err != nil || task == nil || task.ID != "PAY-7" {
		t.Fatalf("%v %v", task, err)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("PAY-7 fits, yet: %s", c.Text)
		}
	}
	if !strings.Contains(strings.Join(jqls, "\n"), "key = PAY-7 AND (assignee = currentUser() AND statusCategory != Done)") {
		t.Errorf("the JQL without ORDER BY: %v", jqls)
	}
	_, checks, err = j.Why(ctx, srv.URL+"/jira/software/projects/PAY/boards/1?selectedIssue=PAY-8")
	if err != nil || checks[len(checks)-1].OK || !strings.Contains(checks[len(checks)-1].Text, "JQL") {
		t.Errorf("PAY-8: %+v %v", checks, err)
	}
}
