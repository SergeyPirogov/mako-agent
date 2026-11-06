package trackers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

// Jira: the issues linked as blocking an issue («is blocked by») and not done are its BlockedBy; a done blocker,
// one it blocks itself and another kind of link are not.
func TestJiraBlockedBy(t *testing.T) {
	var fields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/myself":
			w.Write([]byte(`{"accountId":"me"}`))
		case "/rest/api/3/search/jql":
			var q struct{ Fields []string }
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &q)
			fields = "," + strings.Join(q.Fields, ",") + ","
			w.Write([]byte(`{"issues":[{"key":"PAY-7","fields":{"summary":"S","project":{"key":"PAY"},"issuelinks":[
				{"type":{"name":"Blocks","inward":"is blocked by","outward":"blocks"},"inwardIssue":{"key":"PAY-3","fields":{"status":{"statusCategory":{"key":"indeterminate"}}}}},
				{"type":{"name":"Blocks","inward":"is blocked by","outward":"blocks"},"inwardIssue":{"key":"PAY-4","fields":{"status":{"statusCategory":{"key":"done"}}}}},
				{"type":{"name":"Blocks","inward":"is blocked by","outward":"blocks"},"outwardIssue":{"key":"PAY-9","fields":{"status":{"statusCategory":{"key":"new"}}}}},
				{"type":{"name":"Relates","inward":"relates to","outward":"relates to"},"inwardIssue":{"key":"PAY-5","fields":{"status":{"statusCategory":{"key":"new"}}}}}
			]}}],"isLast":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	j, _ := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	tasks, err := j.FetchMine(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%v %+v", err, tasks)
	}
	if !slices.Equal(tasks[0].BlockedBy, []string{"PAY-3"}) {
		t.Fatalf("blocked by %v, want [PAY-3]", tasks[0].BlockedBy)
	}
	if !strings.Contains(fields, ",issuelinks,") {
		t.Fatalf("issuelinks not asked for: %s", fields)
	}
}

// GitLab: the open issues linked as «is_blocked_by» are BlockedBy; the links are kept while the issue stays the
// same for a while, and read again after; a project without such links (403) has none; a failed read keeps what was
// known (LinksUnread).
func TestGitLabBlockedBy(t *testing.T) {
	reads, status := 0, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/grp%2Fapp/issues":
			w.Write([]byte(`[{"id":1005,"iid":5,"state":"opened","title":"T","web_url":"w","updated_at":"u1"}]`))
		case "/api/v4/projects/grp%2Fapp/issues/5/links":
			reads++
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			w.Write([]byte(`[
				{"iid":3,"state":"opened","link_type":"is_blocked_by","web_url":"https://gl/grp/app/-/issues/3","references":{"full":"grp/app#3"}},
				{"iid":4,"state":"closed","link_type":"is_blocked_by","references":{"full":"grp/app#4"}},
				{"iid":8,"state":"opened","link_type":"is_blocked_by","web_url":"https://gl/grp/lib/-/issues/8"},
				{"iid":6,"state":"opened","link_type":"blocks","references":{"full":"grp/app#6"}},
				{"iid":7,"state":"opened","link_type":"relates_to","references":{"full":"grp/app#7"}}]`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL, Projects: map[string]*config.Project{"grp/app": {Key: "grp/app"}}, TriggerOff: true}, "tok")
	ctx := context.Background()
	fetch := func() ([]string, bool) {
		tasks, err := g.FetchMine(ctx)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("%v %+v", err, tasks)
		}
		return tasks[0].BlockedBy, tasks[0].LinksUnread
	}
	if b, unread := fetch(); !slices.Equal(b, []string{"grp/app#3", "grp/lib#8"}) || unread {
		t.Fatalf("blocked by %v (unread %v)", b, unread)
	}
	fetch()
	if reads != 1 {
		t.Fatalf("links read %d times for an issue that stayed the same", reads)
	}
	// After a while they are read again: a blocker closed elsewhere does not change this issue.
	g.mu.Lock()
	m := g.links["grp/app#5"]
	m.at = m.at.Add(-linksEvery - time.Second)
	g.links["grp/app#5"] = m
	g.mu.Unlock()
	status = 400
	if b, unread := fetch(); !unread || b != nil {
		t.Fatalf("a failed read: blocked by %v, unread %v", b, unread)
	}
	status = 403
	if b, unread := fetch(); unread || len(b) != 0 {
		t.Fatalf("no links in the project: blocked by %v, unread %v", b, unread)
	}
}

// Retarget points an MR/PR into another branch.
func TestRetarget(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" "+string(body))
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL}, "tok")
	if err := g.Retarget(ctx, srv.URL+"/grp/app/-/merge_requests/12", "main"); err != nil {
		t.Fatal(err)
	}
	h, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	if err := h.Retarget(ctx, "https://github.com/acme/web/pull/3", "main"); err != nil {
		t.Fatal(err)
	}
	want := []string{`PUT /api/v4/projects/grp%2Fapp/merge_requests/12 {"target_branch":"main"}`, `PATCH /repos/acme/web/pulls/3 {"base":"main"}`}
	if !slices.Equal(got, want) {
		t.Fatalf("requests %q, want %q", got, want)
	}
	if err := g.Retarget(ctx, "https://github.com/acme/web/pull/3", "main"); err == nil {
		t.Fatal("a PR's URL taken for a GitLab MR")
	}
}
