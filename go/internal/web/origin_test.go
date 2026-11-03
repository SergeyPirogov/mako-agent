package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// Without Origin only a request the browser marks same-origin, or one of Mako's own (X-Mako), gets through.
func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"same Origin", map[string]string{"Origin": "http://127.0.0.1:8080"}, true},
		{"through tailscale serve", map[string]string{"Origin": "https://mac.tail.ts.net", "X-Forwarded-Host": "mac.tail.ts.net"}, true},
		{"another site", map[string]string{"Origin": "https://evil.example"}, false},
		{"another site with X-Mako", map[string]string{"Origin": "https://evil.example", "X-Mako": "1"}, false},
		{"no headers at all", nil, false},
		{"cross-site, no Origin", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"same-origin, no Origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"Mako's command line", map[string]string{"X-Mako": "1"}, true},
		{"X-Mako not 1", map[string]string{"X-Mako": "yes"}, false},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/poll", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := sameOrigin(r); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTokenIs(t *testing.T) {
	for _, c := range []struct {
		got, want string
		ok        bool
	}{{"tok", "tok", true}, {"tok", "other", false}, {"", "", false}, {"tok", "", false}, {"", "tok", false}, {"to", "tok", false}} {
		if tokenIs(c.got, c.want) != c.ok {
			t.Errorf("tokenIs(%q, %q) != %v", c.got, c.want, c.ok)
		}
	}
}

// postRoutes: every POST the package registers, from its source: a literal pattern, the {…} filled in.
func postRoutes(t *testing.T) []string {
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`"POST (/[^"]*)"`)
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			p := m[1]
			switch {
			case strings.HasSuffix(p, "/"):
				continue // its end is added in a loop: below
			case p == "/api/{action}":
				for _, a := range []string{"take", "push", "later", "pick", "cancel", "redo", "say", "plan", "answer", "open",
					"terminalapp", "merge", "close", "lesson", "archive", "clean", "awake", "restart", "show", "poll"} {
					out = append(out, "/api/"+a+"?id=grp%2Fapp%231")
				}
			case strings.Contains(p, "{action}"):
				for _, a := range []string{"report", "clear", "pair", "remove"} {
					out = append(out, strings.Replace(p, "{action}", a, 1))
				}
			default:
				out = append(out, p)
			}
		}
	}
	// The patterns made in loops.
	out = append(out, "/api/update/cancel", "/api/update/now", "/api/code/workspace/reset", "/api/code/workspace/remove")
	if len(out) < 40 || !slices.Contains(out, "/api/settings") || !slices.Contains(out, "/api/open?id=grp%2Fapp%231") {
		t.Fatalf("routes not found in the source: %q", out)
	}
	return out
}

// Another site in the browser cannot press any of the board's buttons: every POST answers it 403.
func TestEveryPostRefusesAnotherSite(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"wipLimit":3}`), 0o600)
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	srv := httptest.NewServer((&Server{D: d, Home: home, Phones: phones, Show: func(string) { t.Error("shown from another site") }}).Handler())
	defer srv.Close()
	for _, p := range postRoutes(t) {
		req, _ := http.NewRequest("POST", srv.URL+p, strings.NewReader(`{}`))
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		want := http.StatusForbidden
		if p == "/api/push/subscribe" {
			want = http.StatusUnauthorized // no paired phone asks: it is refused before the origin is looked at
		}
		if res.StatusCode != want {
			t.Errorf("POST %s from another site: %d, want %d", p, res.StatusCode, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, "mako.config.json")); string(b) != `{"wipLimit":3}` {
		t.Fatalf("the config changed: %s", b)
	}
}
