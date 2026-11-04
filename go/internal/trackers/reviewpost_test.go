package trackers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func TestInDiff(t *testing.T) {
	files := []ReviewFile{
		{Path: "a.go", Status: "M", Diff: "@@ -10,4 +10,5 @@ func x() {\n ctx\n-old\n+new\n+more\n ctx2\n"},
		{Path: "gone.go", Status: "D", Diff: "@@ -1 +0,0 @@\n-x\n"},
	}
	for line, want := range map[int]bool{9: false, 10: true, 11: true, 12: true, 13: true, 14: false, 0: false} {
		if got := inDiff(files, "a.go", line); got != want {
			t.Errorf("a.go:%d: %v", line, got)
		}
	}
	if inDiff(files, "gone.go", 1) || inDiff(files, "b.go", 10) {
		t.Error("a line of a deleted or untouched file")
	}
}

// posts: what a fake code host was sent, by path.
type posts struct {
	mu   sync.Mutex
	list []post
}

type post struct {
	path string
	body map[string]any
}

func (p *posts) add(r *http.Request) map[string]any {
	var b map[string]any
	raw, _ := io.ReadAll(r.Body)
	json.Unmarshal(raw, &b)
	p.mu.Lock()
	p.list = append(p.list, post{r.URL.EscapedPath(), b})
	p.mu.Unlock()
	return b
}

func TestGitLabPostReview(t *testing.T) {
	sent := &posts{}
	failNext := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.EscapedPath(); {
		case r.Method == "GET" && p == "/api/v4/projects/grp%2Fapp/merge_requests/4":
			w.Write([]byte(`{"iid":4,"diff_refs":{"base_sha":"b0","start_sha":"s0","head_sha":"h1"}}`))
		case r.Method == "GET" && p == "/api/v4/projects/grp%2Fapp/merge_requests/4/diffs":
			w.Write([]byte(`[{"old_path":"old.go","new_path":"cache.go","renamed_file":true,"diff":"@@ -1,2 +1,3 @@\n a\n+b\n c\n"}]`))
		case r.Method == "POST" && p == "/api/v4/projects/grp%2Fapp/merge_requests/4/discussions":
			b := sent.add(r)
			if failNext {
				failNext = false
				w.WriteHeader(500)
				w.Write([]byte(`{"message":"boom"}`))
				return
			}
			if _, ok := b["position"]; ok && b["body"] == "rejected line" {
				w.WriteHeader(400)
				w.Write([]byte(`{"message":"line_code can't be blank"}`))
				return
			}
			w.Write([]byte(`{"id":"d1","notes":[{"id":` + map[bool]string{true: "71", false: "72"}[b["position"] != nil] + `}]}`))
		case r.Method == "POST" && p == "/api/v4/projects/grp%2Fapp/merge_requests/4/approve":
			sent.add(r)
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g, _ := NewGitLab(&config.Tracker{ID: "gl", BaseURL: srv.URL}, "tok")
	ctx := context.Background()
	url := "https://gl/grp/app/-/merge_requests/4"
	res, err := g.PostReview(ctx, url, "h1", []DraftNote{
		{Path: "cache.go", Line: 2, Body: "on the line"},
		{Path: "cache.go", Line: 40, Body: "out of the diff"},
		{Body: "about the MR"},
		{Path: "cache.go", Line: 3, Body: "rejected line"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].URL != url+"#note_71" || res[1].URL != url+"#note_72" || res[2].URL != url+"#note_72" || res[3].URL != url+"#note_72" {
		t.Fatalf("results %+v", res)
	}
	if len(sent.list) != 5 {
		t.Fatalf("sent %+v", sent.list)
	}
	pos, _ := sent.list[0].body["position"].(map[string]any)
	if pos["base_sha"] != "b0" || pos["start_sha"] != "s0" || pos["head_sha"] != "h1" || pos["new_path"] != "cache.go" ||
		pos["old_path"] != "old.go" || pos["new_line"] != 2.0 || pos["position_type"] != "text" {
		t.Fatalf("position %+v", pos)
	}
	if b := sent.list[1].body; b["position"] != nil || b["body"] != "`cache.go:40` — out of the diff" {
		t.Fatalf("out of the diff %+v", b)
	}
	if b := sent.list[2].body; b["position"] != nil || b["body"] != "about the MR" {
		t.Fatalf("general %+v", b)
	}
	// A line GitLab refused goes as a general note.
	if b := sent.list[4].body; b["position"] != nil || b["body"] != "`cache.go:3` — rejected line" {
		t.Fatalf("refused %+v", b)
	}

	// A head pushed since the notes were written: no note is put on a line.
	sent.list = nil
	if _, err := g.PostReview(ctx, url, "old", []DraftNote{{Path: "cache.go", Line: 2, Body: "x"}}); err != nil {
		t.Fatal(err)
	}
	if len(sent.list) != 1 || sent.list[0].body["position"] != nil {
		t.Fatalf("stale head %+v", sent.list)
	}

	// A note the host fails on says why; the others go.
	failNext = true
	res, err = g.PostReview(ctx, url, "h1", []DraftNote{{Body: "first"}, {Body: "second"}})
	if err != nil || res[0].Error == "" || res[0].URL != "" || res[1].URL == "" || res[1].Error != "" {
		t.Fatalf("%+v %v", res, err)
	}

	if err := g.ApproveReview(ctx, url); err != nil {
		t.Fatal(err)
	}
	if last := sent.list[len(sent.list)-1]; last.path != "/api/v4/projects/grp%2Fapp/merge_requests/4/approve" {
		t.Fatalf("approve %+v", last)
	}
	if _, err := g.PostReview(ctx, "https://gl/grp/app/-/issues/4", "", nil); err == nil {
		t.Fatal("posted to an issue")
	}
}

func TestGitHubPostReview(t *testing.T) {
	sent := &posts{}
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case r.Method == "GET" && p == "/repos/acme/web/pulls/12/files":
			w.Write([]byte(`[{"filename":"pay.go","status":"modified","patch":"@@ -5,2 +5,3 @@\n a\n+b\n c"}]`))
		case r.Method == "POST" && p == "/repos/acme/web/pulls/12/reviews":
			b := sent.add(r)
			if cs, _ := b["comments"].([]any); refuse && len(cs) > 0 {
				w.WriteHeader(422)
				w.Write([]byte(`{"message":"Unprocessable Entity","errors":["Line could not be resolved"]}`))
				return
			}
			w.Write([]byte(`{"id":5,"html_url":"https://github.com/acme/web/pull/12#pullrequestreview-5"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	ctx := context.Background()
	url := "https://github.com/acme/web/pull/12"
	notes := []DraftNote{{Path: "pay.go", Line: 6, Body: "on the line"}, {Path: "pay.go", Line: 90, Body: "far"}, {Body: "overall"}}
	res, err := g.PostReview(ctx, url, "abc", notes)
	if err != nil {
		t.Fatal(err)
	}
	review := "https://github.com/acme/web/pull/12#pullrequestreview-5"
	for _, r := range res {
		if r.URL != review || r.Error != "" {
			t.Fatalf("results %+v", res)
		}
	}
	b := sent.list[0].body
	cs, _ := b["comments"].([]any)
	if b["event"] != "COMMENT" || b["commit_id"] != "abc" || len(cs) != 1 || b["body"] != "`pay.go:90` — far\n\noverall" {
		t.Fatalf("review %+v", b)
	}
	if c := cs[0].(map[string]any); c["path"] != "pay.go" || c["line"] != 6.0 || c["side"] != "RIGHT" || c["body"] != "on the line" {
		t.Fatalf("comment %+v", c)
	}

	// GitHub will not take a line: all of it goes in the review's body.
	refuse, sent.list = true, nil
	res, err = g.PostReview(ctx, url, "abc", notes)
	if err != nil || res[0].URL != review || len(sent.list) != 2 {
		t.Fatalf("%+v %v %+v", res, err, sent.list)
	}
	if b := sent.list[1].body; !strings.HasPrefix(b["body"].(string), "`pay.go:6` — on the line") || len(b["comments"].([]any)) != 0 {
		t.Fatalf("fallback %+v", b)
	}

	if err := g.ApproveReview(ctx, url); err != nil {
		t.Fatal(err)
	}
	if last := sent.list[len(sent.list)-1].body; last["event"] != "APPROVE" {
		t.Fatalf("approve %+v", last)
	}
}

// A review the host refuses as a whole: each note says why.
func TestGitHubPostReviewFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "gh", APIURL: srv.URL}, "tok")
	res, err := g.PostReview(context.Background(), "https://github.com/acme/web/pull/12", "", []DraftNote{{Body: "x"}})
	if err != nil || len(res) != 1 || res[0].Error == "" || res[0].URL != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if err := g.ApproveReview(context.Background(), "https://github.com/acme/web/pull/12"); err == nil {
		t.Fatal("approved on a 500")
	}
}
