package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
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
	res, err := http.Post(srv.URL+"/api/screenshot?id=grp/app%235&name=after.png", "image/png", bytes.NewReader(png))
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
	res, _ = http.Post(srv.URL+"/api/screenshot?id=nope&name=a.png", "image/png", bytes.NewReader(png))
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
		res, err := http.Post(srv.URL+"/api/attach?"+query, typ, bytes.NewReader(body))
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
}
