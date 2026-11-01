package dispatch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func gitlabAt(t *testing.T, base string) trackers.Tracker {
	t.Helper()
	gl, err := trackers.NewGitLab(&config.Tracker{Type: "gitlab", ID: "gitlab", BaseURL: base, AllProjects: true}, "x")
	if err != nil {
		t.Fatal(err)
	}
	return gl
}

func TestOutageSaysWhichTrackerAndWhy(t *testing.T) {
	ctx := context.Background()
	status := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		defer srv.Close()
		gl := gitlabAt(t, srv.URL)
		_, err := gl.FetchMine(ctx)
		if err == nil {
			t.Fatalf("%d: no error", code)
		}
		return outage(gl, err)
	}
	if got := status(503); got != "GitLab · 127.0.0.1: the service is down (503), will try later" {
		t.Errorf("503: %q", got)
	}
	if got := status(401); !strings.Contains(got, "check the token") {
		t.Errorf("401: %q", got)
	}

	// Nothing listens: the service is down (or behind a VPN that is off).
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	gl := gitlabAt(t, "http://"+addr)
	_, err := gl.FetchMine(ctx)
	if got := outage(gl, err); !strings.HasPrefix(got, "GitLab · 127.0.0.1: the service refuses connections") {
		t.Errorf("refused: %q (%v)", got, err)
	}

	// The host is not found (no internet, the VPN's DNS is gone): the host from the request is named.
	dns := &url.Error{Op: "Get", URL: "https://git.example.app/api/v4/issues", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "git.example.app", IsNotFound: true}}}
	if got := outage(gitlabAt(t, "https://git.example.app"), dns); got != "GitLab · git.example.app: server not found — check the internet or VPN" {
		t.Errorf("dns: %q", got)
	}
	timeout := &url.Error{Op: "Get", URL: "https://git.example.app/x", Err: context.DeadlineExceeded}
	if got := outageReason(timeout); !strings.Contains(got, "is not responding") {
		t.Errorf("timeout: %q", got)
	}
}

func TestPollKeepsWhenTrackerWentDown(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	if s := d.Searches()["gitlab"]; s.Error != "" || s.Down != "" || !s.Since.IsZero() {
		t.Fatalf("up: %+v", s)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	d.Trackers = []trackers.Tracker{gitlabAt(t, "http://"+addr)}
	d.Poll(ctx)
	first := d.Searches()["gitlab"]
	if first.Down == "" || first.Since.IsZero() {
		t.Fatalf("down: %+v", first)
	}
	time.Sleep(5 * time.Millisecond)
	d.Poll(ctx)
	if again := d.Searches()["gitlab"]; !again.Since.Equal(first.Since) || !again.At.After(first.At) {
		t.Errorf("still down since the first failure: %+v, first %+v", again, first)
	}
}

// GitHub answers a search for an unknown assignee with 422: the board names the login, not «помилка 422».
func TestOutageNamesUnknownAssignee(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed","errors":[{"value":"SergeyPirgov","resource":"Issue","field":"assignee","code":"invalid"}]}`))
	}))
	defer srv.Close()
	gh, err := trackers.NewGitHub(&config.Tracker{Type: "github", ID: "github", APIURL: srv.URL, AllProjects: true, Assignee: "SergeyPirgov"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	_, err = gh.FetchMine(context.Background())
	if got := outage(gh, err); !strings.Contains(got, "user \"SergeyPirgov\" does not exist") {
		t.Errorf("got %q (%v)", got, err)
	}
}
