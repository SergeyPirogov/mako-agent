package trackers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

// A GitLab issue with more notes than one page holds: the agent gets the notes of the second page too, the
// system ones left out.
func TestGitLabNotesPages(t *testing.T) {
	const total = perPage + 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/grp%2Fapp/issues":
			fmt.Fprintf(w, `[{"id":1005,"iid":5,"state":"opened","title":"T","web_url":"w","user_notes_count":%d,"updated_at":"u1"}]`, total)
		case "/api/v4/projects/grp%2Fapp/issues/5/notes":
			if r.URL.Query().Get("sort") != "asc" {
				t.Errorf("notes not in the order written: %q", r.URL.RawQuery)
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			items := []any{}
			for n := (page-1)*perPage + 1; n <= min(page*perPage, total); n++ {
				items = append(items, map[string]any{"body": fmt.Sprintf("n%d", n), "system": n == 3, "author": map[string]any{"id": 7, "username": "ann", "name": "Ann"}})
			}
			json.NewEncoder(w).Encode(items)
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL, Projects: map[string]*config.Project{"grp/app": {Key: "grp/app"}}, TriggerOff: true}, "tok")
	tasks, err := g.FetchMine(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%v %+v", err, tasks)
	}
	cs := tasks[0].Comments
	if len(cs) != total-1 || cs[len(cs)-1].Body != fmt.Sprintf("n%d", total) || tasks[0].CommentsUnread {
		t.Fatalf("%d comments, unread %v", len(cs), tasks[0].CommentsUnread)
	}
}

// jiraCommentsServer: one issue PAY-1 with total comments; the search embeds only the first `embedded` of them,
// the rest come from the issue's comment list, page by page. failList makes that list fail.
func jiraCommentsServer(t *testing.T, total, embedded int, failList bool, reads *int) *httptest.Server {
	comment := func(n int) map[string]any {
		body := map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph",
			"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("c%d", n)}}}}}
		return map[string]any{"body": body, "author": map[string]any{"accountId": "a1", "displayName": "Ann", "accountType": "atlassian"}}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/myself":
			w.Write([]byte(`{"accountId":"acc-1"}`))
		case "/rest/api/3/search/jql":
			var cs []any
			for n := 1; n <= embedded; n++ {
				cs = append(cs, comment(n))
			}
			json.NewEncoder(w).Encode(map[string]any{"isLast": true, "issues": []any{map[string]any{"key": "PAY-1", "fields": map[string]any{
				"summary": "T", "project": map[string]any{"key": "PAY"}, "comment": map[string]any{"comments": cs, "total": total, "maxResults": embedded}}}}})
		case "/rest/api/3/issue/PAY-1/comment":
			*reads++
			if failList {
				http.Error(w, "boom", 500)
				return
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
			var cs []any
			for n := start + 1; n <= min(start+perPage, total); n++ {
				cs = append(cs, comment(n))
			}
			json.NewEncoder(w).Encode(map[string]any{"startAt": start, "total": total, "comments": cs})
		default:
			http.NotFound(w, r)
		}
	}))
}

// Jira embeds only a page of an issue's comments in the search; the rest are read from the comment list.
func TestJiraCommentsPages(t *testing.T) {
	const total = 2*perPage + 30
	reads := 0
	srv := jiraCommentsServer(t, total, 50, false, &reads)
	defer srv.Close()
	j, _ := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	tasks, err := j.FetchMine(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%v %+v", err, tasks)
	}
	cs := tasks[0].Comments
	if len(cs) != total || cs[0].Body != "c1" || cs[total-1].Body != fmt.Sprintf("c%d", total) || tasks[0].CommentsUnread || reads != 2 {
		t.Fatalf("%d comments, unread %v, %d reads", len(cs), tasks[0].CommentsUnread, reads)
	}
	for i, c := range cs {
		if c.Body != fmt.Sprintf("c%d", i+1) {
			t.Fatalf("comment %d is %q: lost or doubled", i, c.Body)
		}
	}
}

// All the comments in the search's page: no extra request. The comment list failing keeps what the search gave
// and marks the comments as not fully read.
func TestJiraCommentsComplete(t *testing.T) {
	reads := 0
	srv := jiraCommentsServer(t, 3, 3, false, &reads)
	defer srv.Close()
	j, _ := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	tasks, err := j.FetchMine(context.Background())
	if err != nil || len(tasks) != 1 || len(tasks[0].Comments) != 3 || reads != 0 {
		t.Fatalf("%v %+v, %d reads", err, tasks, reads)
	}

	srv2 := jiraCommentsServer(t, 150, 50, true, &reads)
	defer srv2.Close()
	j, _ = NewJira(&config.Tracker{ID: "jira", BaseURL: srv2.URL}, "me@x", "tok")
	tasks, err = j.FetchMine(context.Background())
	if err != nil || len(tasks) != 1 || len(tasks[0].Comments) != 50 || !tasks[0].CommentsUnread {
		t.Fatalf("failed list: %v %d comments, unread %v", err, len(tasks[0].Comments), tasks[0].CommentsUnread)
	}
}
