package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// A message to the agent never vanishes quietly: a plain-text 404 (the task is gone), a 502 from sw.js, a rejected
// fetch (Mako restarting) all show the warning and give the text and the pictures back to the field. postJSON,
// sayTo and restoreUnsent are cut out of board.html and run in node (skipped without node).
func TestSayFailureKeepsTheMessage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	var parts []string
	for _, re := range []string{`(?s)\nasync function postJSON\(.*?\n}\n`, `(?s)\nasync function sayTo\(.*?\n}\n`, `(?s)\nfunction restoreUnsent\(.*?\n}\n`} {
		m := regexp.MustCompile(re).FindString(string(board))
		if m == "" {
			t.Fatalf("board.html has no %s", re)
		}
		parts = append(parts, m)
	}
	words, _ := json.Marshal(i18n.WebStrings())
	script := `
const words = ` + string(words) + `;
const t = (k, v = {}) => (words[k] ?? k).replace(/\{(\w+)\}/g, (_, n) => v[n]);
` + strings.Join(parts, "\n") + `
const reply = (status, body, ctype = "text/plain") => async () => ({ ok: status < 300, status, headers: { get: () => ctype }, json: async () => JSON.parse(body) });
const cases = {
  notFound: reply(404, "no such task"),
  gateway: reply(502, "bad gateway"),
  rejected: async () => { throw new TypeError("Failed to fetch"); },
  serverError: reply(500, '{"ok":false,"error":"boom"}', "application/json"),
  good: reply(200, '{"ok":true}', "application/json"),
  noOkJSON: reply(500, "{}", "application/json"),
};
(async () => {
  const out = {};
  for (const [k, f] of Object.entries(cases)) {
    globalThis.fetch = f;
    const shown = [];
    const sent = await sayTo("x", "hello", (m) => shown.push(m));
    const v = { input: { value: "" }, att: [] };
    if (!sent) restoreUnsent(v, "hello", [{ line: "pic" }]);
    out[k] = { sent, warn: shown[shown.length - 1], field: v.input.value, att: v.att.length };
  }
  // text typed while the request ran stays, the unsent one goes before it
  const v = { input: { value: "new" }, att: [{ line: "b" }] };
  restoreUnsent(v, "old", [{ line: "a" }]);
  out.merged = { field: v.input.value, att: v.att.map((a) => a.line).join() };
  // an attach failure says nothing about a "message"
  globalThis.fetch = cases.notFound;
  out.attach = await postJSON("/api/attach?id=x", {}, { failed: t("b.attach.failed"), gone: t("b.attach.gone") });
  console.log(JSON.stringify(out));
})();
`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var out map[string]struct {
		Sent  bool
		Warn  string
		Field string
		Att   any
		Error string
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("node printed %q: %v", b, err)
	}
	for _, k := range []string{"notFound", "gateway", "rejected", "serverError", "noOkJSON"} {
		o := out[k]
		if o.Sent || o.Warn == "" || o.Field != "hello" || o.Att != float64(1) {
			t.Errorf("%s: want a warning and the text and picture back in the field, got %+v", k, o)
		}
	}
	if w := out["notFound"].Warn; !strings.Contains(w, "task is gone") {
		t.Errorf("notFound warning: %q", w)
	}
	if w := out["serverError"].Warn; w != "boom" {
		t.Errorf("serverError warning: %q", w)
	}
	if o := out["good"]; !o.Sent || o.Warn != "" || o.Field != "" {
		t.Errorf("good: %+v", o)
	}
	if o := out["merged"]; o.Field != "old\nnew" {
		t.Errorf("merged: %+v", o)
	}
	if e := out["attach"].Error; !strings.Contains(e, "picture") || strings.Contains(e, "message") {
		t.Errorf("attach 404 text: %q", e)
	}
}
