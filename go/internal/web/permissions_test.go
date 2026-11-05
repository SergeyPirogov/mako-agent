package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/setup"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// Settings → Permissions: the journal by rule, «Allow always» keeps a rule for every agent after (never one Mako
// may not keep), «Remove» takes one away, «Dismiss» hides a rule; only from the board's own pages.
func TestPermissionsEndpoints(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"executors":{"terminal":{"type":"terminal","allowedTools":["Bash(git add:*)"]}}}`), 0o600)
	cfg, ts, _ := setup.Build(home)
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(cfg, store, ts)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()

	type view struct {
		OK      bool                 `json:"ok"`
		Error   string               `json:"error"`
		Groups  []dispatch.PermGroup `json:"groups"`
		Allowed []string             `json:"allowed"`
	}
	get := func() view {
		t.Helper()
		res, err := http.Get(srv.URL + "/api/settings/permissions")
		if err != nil {
			t.Fatal(err)
		}
		var v view
		json.NewDecoder(res.Body).Decode(&v)
		return v
	}
	post := func(origin, body string) (int, view) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/api/settings/permissions", strings.NewReader(body))
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var v view
		json.NewDecoder(res.Body).Decode(&v)
		return res.StatusCode, v
	}
	rules := func(v view) string {
		var out []string
		for _, g := range v.Groups {
			out = append(out, g.Rule)
		}
		return strings.Join(out, " ")
	}

	d.NotePermission("a#1", "Bash", json.RawMessage(`{"command":"git add -A && go vet ./..."}`), []string{"Bash(git add:*)", "Bash(go vet:*)"})
	d.NotePermission("a#1", "WebFetch", json.RawMessage(`{"url":"https://x"}`), nil)
	if v := get(); !v.OK || rules(v) != "Bash(go vet:*) WebFetch" || strings.Join(v.Allowed, " ") != "Bash(git add:*)" {
		t.Fatalf("journal %+v", v)
	}
	if code, _ := post("https://evil.example", `{"allow":["Bash(rm:*)"]}`); code != http.StatusForbidden {
		t.Fatalf("from another site: %d", code)
	}
	if _, v := post(srv.URL, `{"allow":["Bash(:*)"]}`); v.OK || !strings.Contains(v.Error, "not a rule Mako can keep") {
		t.Fatalf("a rule for any command: %+v", v)
	}
	if _, v := post(srv.URL, `{"allow":["Bash(go vet:*)"]}`); !v.OK || strings.Join(v.Allowed, " ") != "Bash(git add:*) Bash(go vet:*)" {
		t.Fatalf("allow: %+v", v)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "mako.config.json")); !strings.Contains(string(b), `"Bash(go vet:*)"`) {
		t.Fatalf("not saved: %s", b)
	}
	if v := get(); rules(v) != "WebFetch" {
		t.Errorf("an allowed rule still asked: %+v", v)
	}
	if _, v := post(srv.URL, `{"remove":["Bash(git add:*)"],"dismiss":["WebFetch"]}`); !v.OK || strings.Join(v.Allowed, " ") != "Bash(go vet:*)" {
		t.Fatalf("remove: %+v", v)
	}
	// The rule taken away is asked for again; the dismissed one is not shown.
	if v := get(); rules(v) != "Bash(git add:*)" {
		t.Errorf("after remove and dismiss: %+v", v)
	}
}

// «Always allow selected» sends the rules ticked: each must be one Mako may keep, and at least one is ticked; no
// body (a notification's button) keeps all the request suggests.
func TestChosenRules(t *testing.T) {
	for _, c := range []struct{ body, want, err string }{
		{"", "<nil>", ""},
		{`{}`, "<nil>", ""},
		{`{"rules":[" Bash(go vet:*) ","","WebFetch"]}`, "Bash(go vet:*) WebFetch", ""},
		{`{"rules":[]}`, "", "no rule is ticked"},
		{`{"rules":["Bash(.:*)"]}`, "", "not a rule Mako can keep"},
		{`{"rules":`, "", "could not read the rules"},
		{`{"rules":["` + strings.Repeat("x", 17<<10) + `"]}`, "", errTooLong},
	} {
		got, err := chosenRules(httptest.NewRequest("POST", "/api/permit", strings.NewReader(c.body)))
		s := strings.Join(got, " ")
		if got == nil {
			s = "<nil>"
		}
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%.40q: error %v, want %q", c.body, err, c.err)
		case c.err == "" && (err != nil || s != c.want):
			t.Errorf("%.40q: %q %v, want %q", c.body, s, err, c.want)
		}
	}
}
