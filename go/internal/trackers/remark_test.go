package trackers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// The reviewer's comments: in the issue with the link to the comment, and in the MR, on each host.
func TestCommentLinks(t *testing.T) {
	posted := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &b)
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		posted[r.URL.Path] = string(raw)
		switch r.URL.Path {
		case "/repos/acme/web/issues/3/comments":
			w.Write([]byte(`{"id":1,"html_url":"https://github.com/acme/web/issues/3#issuecomment-1"}`))
		case "/repos/acme/web/issues/12/comments":
			w.Write([]byte(`{"id":2,"html_url":"https://github.com/acme/web/pull/12#issuecomment-2"}`))
		case "/api/v4/projects/grp%2Fapp/issues/5/notes", "/api/v4/projects/grp/app/issues/5/notes":
			w.Write([]byte(`{"id":41}`))
		case "/api/v4/projects/grp%2Fapp/merge_requests/9/notes", "/api/v4/projects/grp/app/merge_requests/9/notes":
			w.Write([]byte(`{"id":42}`))
		case "/rest/api/3/issue/PAY-7/comment":
			w.Write([]byte(`{"id":"10001"}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	gh, _ := NewGitHub(&config.Tracker{ID: "github", APIURL: srv.URL}, "tok")
	if u, err := gh.CommentLink(ctx, core.Task{ID: "acme/web#3", Project: "acme/web"}, "R1"); err != nil || u != "https://github.com/acme/web/issues/3#issuecomment-1" {
		t.Errorf("github issue: %q %v", u, err)
	}
	if u, err := gh.CommentReview(ctx, "https://github.com/acme/web/pull/12", "R1"); err != nil || u != "https://github.com/acme/web/pull/12#issuecomment-2" {
		t.Errorf("github PR: %q %v", u, err)
	}

	gl, _ := NewGitLab(&config.Tracker{ID: "gitlab", BaseURL: srv.URL}, "tok")
	if u, err := gl.CommentLink(ctx, core.Task{ID: "grp/app#5", Project: "grp/app", URL: "https://gl/grp/app/-/issues/5"}, "R1"); err != nil || u != "https://gl/grp/app/-/issues/5#note_41" {
		t.Errorf("gitlab issue: %q %v", u, err)
	}
	if u, err := gl.CommentReview(ctx, "https://gl/grp/app/-/merge_requests/9", "R1"); err != nil || u != "https://gl/grp/app/-/merge_requests/9#note_42" {
		t.Errorf("gitlab MR: %q %v", u, err)
	}
	if _, err := gl.CommentReview(ctx, "https://gl/grp/app/issues/9", "R1"); err == nil {
		t.Error("not an MR")
	}

	j, _ := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	if u, err := j.CommentLink(ctx, core.Task{ID: "PAY-7"}, "R1"); err != nil || u != srv.URL+"/browse/PAY-7?focusedCommentId=10001" {
		t.Errorf("jira: %q %v", u, err)
	}
	if len(posted) != 5 {
		t.Errorf("posted %v", posted)
	}
}
