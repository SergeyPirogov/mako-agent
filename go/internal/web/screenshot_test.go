package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// `mako screenshot` hands a task's screenshot to Mako: kept for the board, the upload's failure said, not fatal.
func TestScreenshot(t *testing.T) {
	dir := t.TempDir()
	store, _ := state.Open(dir)
	store.Update("grp/app#5", func(r *core.Record) {
		r.Status, r.Task = core.Snoozed, core.Task{ID: "grp/app#5", Title: "Pay", TrackerID: "jira", Project: "grp/app"}
	})
	d := dispatch.New(&config.Config{StateDir: dir}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	png := []byte("\x89PNG\r\n\x1a\nfake")
	res, err := postMako(srv.URL+"/api/screenshot?id=grp/app%235&name=after.png", "image/png", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		OK          bool   `json:"ok"`
		URL         string `json:"url"`
		Markdown    string `json:"markdown"`
		UploadError string `json:"uploadError"`
	}
	json.NewDecoder(res.Body).Decode(&reply)
	if !reply.OK || !strings.HasPrefix(reply.URL, "/api/screenshots/grp-app-5/") || !strings.HasSuffix(reply.URL, "-after.png") || reply.UploadError == "" || reply.Markdown != "" {
		t.Fatalf("reply %+v", reply)
	}
	res, _ = http.Get(srv.URL + reply.URL)
	if got, _ := io.ReadAll(res.Body); res.StatusCode != 200 || !bytes.Equal(got, png) {
		t.Fatalf("served %d %q", res.StatusCode, got)
	}

	// An unknown task, a foreign page and a path out of the folder are refused.
	res, _ = postMako(srv.URL+"/api/screenshot?id=nope&name=a.png", "image/png", bytes.NewReader(png))
	json.NewDecoder(res.Body).Decode(&reply)
	if reply.OK {
		t.Fatal("unknown task accepted")
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/screenshot?id=grp/app%235&name=a.png", bytes.NewReader(png))
	req.Header.Set("Origin", "https://evil.example")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("cross-site: %d", res.StatusCode)
	}
	if res, _ := http.Get(srv.URL + "/api/screenshots/..%2F/state.json"); res.StatusCode == 200 {
		t.Fatal("served a file outside the screenshots")
	}
}

// A picture attached to a message for the agent: kept beside the task's screenshots, named in the message by its
// path, served to the board; anything else, too large or for no task is refused.
func TestAttach(t *testing.T) {
	dir := t.TempDir()
	store, _ := state.Open(dir)
	store.Update("grp/app#5", func(r *core.Record) { r.Task = core.Task{ID: "grp/app#5", Title: "Pay"} })
	os.WriteFile(filepath.Join(dir, "questions.json"), []byte(`[{"id":"ask-abc123","project":"t/p/r","title":"Where?"}]`), 0o600)
	d := dispatch.New(&config.Config{StateDir: dir}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	type reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Path  string `json:"path"`
		URL   string `json:"url"`
		Line  string `json:"line"`
	}
	post := func(query, typ string, body []byte) reply {
		t.Helper()
		res, err := postMako(srv.URL+"/api/attach?"+query, typ, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var r reply
		json.NewDecoder(res.Body).Decode(&r)
		return r
	}

	png := []byte("\x89PNG\r\n\x1a\nfake")
	r := post("id=grp/app%235&name=shot.png", "image/png", png)
	if !r.OK || !strings.HasPrefix(r.URL, "/api/screenshots/grp-app-5/") || !strings.HasSuffix(r.URL, "-shot.png") ||
		!strings.HasPrefix(r.Path, dir) || r.Line != "[Image: "+r.Path+"]" {
		t.Fatalf("reply %+v", r)
	}
	if got, err := os.ReadFile(r.Path); err != nil || !bytes.Equal(got, png) {
		t.Fatalf("kept %q %v", got, err)
	}
	res, _ := http.Get(srv.URL + r.URL)
	if got, _ := io.ReadAll(res.Body); res.StatusCode != 200 || !bytes.Equal(got, png) {
		t.Fatalf("served %d %q", res.StatusCode, got)
	}

	if r := post("id=grp/app%235&name=a.txt", "text/plain", []byte("hi")); r.OK {
		t.Fatal("a text file accepted")
	}
	if r := post("id=grp/app%235&name=big.png", "image/png", make([]byte, maxImage+1)); r.OK || !strings.Contains(r.Error, "10 MB") {
		t.Fatalf("too large: %+v", r)
	}
	if r := post("id=nope&name=a.png", "image/png", png); r.OK {
		t.Fatal("unknown task accepted")
	}

	// A question about code: its own, and a new one that has no id yet.
	for _, id := range []string{"ask-abc123", "ask-new"} {
		r := post("id="+id+"&name=err.png", "image/png", png)
		if !r.OK || !strings.HasPrefix(r.URL, "/api/screenshots/"+id+"/") || !strings.HasPrefix(r.Path, dir) || r.Line != "[Image: "+r.Path+"]" {
			t.Fatalf("%s: reply %+v", id, r)
		}
		if got, err := os.ReadFile(r.Path); err != nil || !bytes.Equal(got, png) {
			t.Fatalf("%s: kept %q %v", id, got, err)
		}
		res, _ := http.Get(srv.URL + r.URL)
		if got, _ := io.ReadAll(res.Body); res.StatusCode != 200 || !bytes.Equal(got, png) {
			t.Fatalf("%s: served %d %q", id, res.StatusCode, got)
		}
	}
	if r := post("id=ask-gone&name=a.png", "image/png", png); r.OK {
		t.Fatal("an unknown question accepted")
	}
}

// A picture in a GitLab issue's text goes through Mako, which holds the token: a picture is served as one, the
// tracker's login page, a path that is no upload, an unknown tracker and a failed fetch are refused.
func TestTrackerImage(t *testing.T) {
	const secret = "35e4b8215c97b932f4083d09e557b2d0"
	png := []byte("\x89PNG\r\n\x1a\nfake")
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/grp%2Fapp/uploads/" + secret + "/image.png":
			w.Write(png)
		case "/api/v4/projects/grp%2Fapp/uploads/" + secret + "/login.png":
			w.Write([]byte("<html>Sign in</html>"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	g, _ := trackers.NewGitLab(&config.Tracker{ID: "gl", BaseURL: gl.URL}, "tok")
	dir := t.TempDir()
	store, _ := state.Open(dir)
	d := dispatch.New(&config.Config{StateDir: dir}, store, []trackers.Tracker{g})
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	get := func(tracker, project, path string) (*http.Response, []byte) {
		res, err := http.Get(srv.URL + "/api/tracker-image?tracker=" + url.QueryEscape(tracker) + "&project=" + url.QueryEscape(project) + "&path=" + url.QueryEscape(path))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, body
	}
	res, body := get("gl", "grp/app", "/uploads/"+secret+"/image.png")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, png) {
		t.Fatalf("picture: %d %s %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	for name, c := range map[string][3]string{
		"a login page":    {"gl", "grp/app", "/uploads/" + secret + "/login.png"},
		"not an upload":   {"gl", "grp/app", "/api/v4/user"},
		"unknown tracker": {"nope", "grp/app", "/uploads/" + secret + "/image.png"},
		"gone":            {"gl", "grp/app", "/uploads/" + secret + "/gone.png"},
	} {
		if res, body := get(c[0], c[1], c[2]); res.StatusCode < 400 || bytes.Contains(body, []byte("Sign in")) {
			t.Errorf("%s: %d %q", name, res.StatusCode, body)
		}
	}
}
