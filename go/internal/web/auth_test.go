package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// The board hears of a failed login in /api/state; «I logged in — continue» lifts the hold from the Mac only, and
// a second press (or another site's) changes nothing.
func TestAuthHoldOnTheBoard(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	held := func() bool {
		req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
		req.Header.Set("X-Mako", "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var got struct {
			AuthHold *struct {
				Text string `json:"text"`
			} `json:"authHold"`
		}
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.AuthHold != nil && got.AuthHold.Text == "Failed to authenticate"
	}
	post := func(origin, from string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/auth/continue", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if from != "" {
			req.Header.Set("X-Forwarded-For", from)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if held() {
		t.Fatal("held before anything failed")
	}
	d.NoteAuth(true, "Failed to authenticate")
	if !held() {
		t.Fatal("/api/state says nothing of the failed login")
	}
	if code := post("https://evil.example", ""); code != http.StatusForbidden || !held() {
		t.Fatalf("from another site: %d", code)
	}
	if code := post(srv.URL, "100.64.0.2"); code == http.StatusOK || !held() {
		t.Fatalf("from a phone: %d", code)
	}
	if code := post(srv.URL, ""); code != http.StatusOK || held() {
		t.Fatalf("from the Mac: %d, still held: %v", code, held())
	}
}

// The words of the login card and banner come from the strings, and the card shows Claude's own words with their
// backticks as code, not raw. The functions are cut out of board.html and run in node (skipped without node).
func TestAuthCardAndBanner(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(board)
	var parts []string
	for _, re := range []string{`(?m)^const esc = .*$`, `(?m)^const MD_IMG = .*$`, `(?m)^const MD_LINK = .*$`, `(?m)^const pic = .*$`, `(?s)\nfunction md\(text\) \{.*?\n}\n`, `(?s)\nfunction authHoldHTML\(.*?\n}\n`, `(?s)\nfunction chatItem\(.*?\n}\n`} {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("board.html has no %s", re)
		}
		parts = append(parts, m)
	}
	words, _ := json.Marshal(i18n.WebStrings())
	script := `
const words = ` + string(words) + `;
const t = (k, v = {}) => (words[k] ?? k).replace(/\{(\w+)\}/g, (_, n) => v[n]);
const data = { records: [{ status: "queued" }, { status: "queued", archivedAt: 1 }] };
const ICON = {}, MR_ICON = {};
` + strings.Join(parts, "\n") + `
const it = (kind, text) => ({ kind, text });
const v = { flip: new Set() };
console.log(JSON.stringify({
  banner: authHoldHTML({ at: 1, text: "x" }),
  none: authHoldHTML(null),
  auth: chatItem(it("auth", "Failed to authenticate: run ` + "`claude auth login`" + `"), 0, v),
  error: chatItem(it("error", "Run ` + "`claude auth login`" + ` now"), 0, v),
}));
`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("node printed %q: %v", b, err)
	}
	if out["none"] != "" {
		t.Errorf("a banner with no hold: %q", out["none"])
	}
	for name, wants := range map[string][]string{
		"banner": {"Claude needs you to log in again", "(1 queued)", "data-auth-go", `data-copy-cmd="claude auth login"`, "<code>claude auth login</code>"},
		"auth":   {`class="notice auth"`, "Claude needs you to log in again", `data-copy-cmd="claude auth login"`, "<code>claude setup-token</code>", "<code>CLAUDE_CODE_OAUTH_TOKEN</code>", "<code>claude auth login</code>"},
		"error":  {"<code>claude auth login</code>"},
	} {
		for _, want := range wants {
			if !strings.Contains(out[name], want) {
				t.Errorf("%s: no %q in %q", name, want, out[name])
			}
		}
	}
	if strings.Contains(out["error"], "`") || strings.Contains(out["auth"], "`") {
		t.Errorf("raw backticks left: %q / %q", out["error"], out["auth"])
	}
}
