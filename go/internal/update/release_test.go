package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func releaseAPI(t *testing.T, tag string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("sent a token")
		}
		w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://github.com/SergeyPirogov/mako-agent/releases/tag/` + tag + `","body":"## Що нового\r\n\r\nБанер про нові релізи.\r\n- ще щось"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestReleaseNewerSameOlder(t *testing.T) {
	for _, c := range []struct {
		current, tag string
		show         bool
	}{
		{"0.3.0", "v0.4.0", true},
		{"v0.3.0", "v0.3.1", true},
		{"0.9.0", "v0.10.0", true},
		{"0.3.0", "v0.3.0", false},
		{"0.4.0", "v0.3.9", false},
		{"0.3.0", "v0.4.0-rc1", false},
	} {
		srv, _ := releaseAPI(t, c.tag)
		r := &Releases{Current: c.current, API: srv.URL}
		if err := r.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := r.Latest()
		if (got != nil) != c.show {
			t.Fatalf("%s → %s: %+v", c.current, c.tag, got)
		}
		if got != nil && (got.Version != c.tag[1:] || got.URL == "" || got.Notes != "Що нового Банер про нові релізи. - ще щось") {
			t.Fatalf("release %+v", got)
		}
	}
}

func TestReleaseNotCheckedForCloneBuilds(t *testing.T) {
	for _, v := range []string{"v0.2.1-35-gd4ae433", "d4ae433", "dev", ""} {
		srv, calls := releaseAPI(t, "v9.0.0")
		r := &Releases{Current: v, API: srv.URL}
		r.Check(context.Background())
		if calls.Load() != 0 || r.Latest() != nil {
			t.Fatalf("%q: asked GitHub %d times", v, calls.Load())
		}
	}
}

func TestReleaseAPIDownIsQuiet(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer limited.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	for _, api := range []string{limited.URL, down.URL} {
		r := &Releases{Current: "0.3.0", API: api}
		if err := r.Check(context.Background()); err == nil {
			t.Fatalf("%s: no error", api)
		}
		if r.Latest() != nil {
			t.Fatalf("%s: a banner", api)
		}
	}
}

func TestReleaseDismissedUntilTheNextOne(t *testing.T) {
	file := filepath.Join(t.TempDir(), "release.json")
	srv, _ := releaseAPI(t, "v0.4.0")
	r := &Releases{Current: "0.3.0", API: srv.URL, File: file}
	r.Check(context.Background())
	if r.Latest() == nil {
		t.Fatal("no banner")
	}
	if err := r.Dismiss("0.4.0"); err != nil {
		t.Fatal(err)
	}
	if r.Latest() != nil {
		t.Fatal("shown after hiding")
	}
	// After a restart it stays hidden…
	r = &Releases{Current: "0.3.0", API: srv.URL, File: file}
	r.Check(context.Background())
	if r.Latest() != nil {
		t.Fatal("shown again after a restart")
	}
	// …until a later release is out.
	next, _ := releaseAPI(t, "v0.5.0")
	r.API = next.URL
	r.Check(context.Background())
	if got := r.Latest(); got == nil || got.Version != "0.5.0" {
		t.Fatalf("next release: %+v", got)
	}
}

func TestReleaseFoundIgnoresDismissAndKeepsTheCheck(t *testing.T) {
	srv, _ := releaseAPI(t, "v0.4.0")
	r := &Releases{Current: "0.3.0", API: srv.URL, File: filepath.Join(t.TempDir(), "release.json")}
	if at, why := r.Checked(); at != 0 || why != "" {
		t.Fatalf("before the first look: %d %q", at, why)
	}
	r.Check(context.Background())
	r.Dismiss("0.4.0")
	if r.Latest() != nil {
		t.Fatal("banner shown after hiding")
	}
	if got := r.Found(); got == nil || got.Version != "0.4.0" || got.URL == "" {
		t.Fatalf("found %+v", got)
	}
	if at, why := r.Checked(); at == 0 || why != "" {
		t.Fatalf("after a look: %d %q", at, why)
	}
	// A failed look says why, keeps the time of the last good one and does not lose the release found.
	at, _ := r.Checked()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "limit", http.StatusForbidden)
	}))
	defer down.Close()
	r.API = down.URL
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if got, why := r.Checked(); got != at || why == "" {
		t.Fatalf("after a failed look: %d %q", got, why)
	}
	if r.Found() == nil {
		t.Fatal("a failed look lost the release")
	}
}
