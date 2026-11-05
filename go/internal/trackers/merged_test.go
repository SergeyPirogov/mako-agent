package trackers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func TestMergedRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/me/app/pulls/12":
			w.Write([]byte(`{"number":12,"base":{"sha":"b1"},"head":{"sha":"h1"}}`))
		case r.URL.Path == "/repos/me/app/pulls/13":
			w.Write([]byte(`{"number":13,"base":{},"head":{"sha":"h1"}}`))
		case r.URL.EscapedPath() == "/api/v4/projects/grp%2Fapp/merge_requests/4":
			w.Write([]byte(`{"iid":4,"diff_refs":{"base_sha":"b2","start_sha":"s2","head_sha":"h2"}}`))
		case r.URL.EscapedPath() == "/api/v4/projects/grp%2Fapp/merge_requests/5":
			w.Write([]byte(`{"iid":5,"diff_refs":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	gh, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	got, err := gh.MergedRefs(ctx, "https://github.com/me/app/pull/12")
	if err != nil || got != (MergedRefs{Base: "b1", Head: "h1", Ref: "refs/pull/12/head"}) {
		t.Errorf("github: %+v %v", got, err)
	}
	if _, err := gh.MergedRefs(ctx, "https://github.com/me/app/pull/13"); err == nil {
		t.Error("github: a PR without its base must be an error")
	}
	if _, err := gh.MergedRefs(ctx, "https://gitlab.com/grp/app/-/merge_requests/4"); err == nil {
		t.Error("github: a GitLab URL must be an error")
	}
	gl, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL}, "tok")
	got, err = gl.MergedRefs(ctx, srv.URL+"/grp/app/-/merge_requests/4")
	if err != nil || got != (MergedRefs{Base: "b2", Head: "h2", Ref: "refs/merge-requests/4/head"}) {
		t.Errorf("gitlab: %+v %v", got, err)
	}
	if _, err := gl.MergedRefs(ctx, srv.URL+"/grp/app/-/merge_requests/5"); err == nil {
		t.Error("gitlab: an MR without diff_refs must be an error")
	}
	var _ RefsHost = gh
	var _ RefsHost = gl
}
