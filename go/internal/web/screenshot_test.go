package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
