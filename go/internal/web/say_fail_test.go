package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"testing"
)

// A message to the agent never vanishes quietly: a plain-text 404 (the task is gone), a 502 from sw.js, a rejected
// fetch (Mako restarting) all come back from postJSON as {ok:false, error}, which say() turns into the warning and
// the text back in the field. postJSON is cut out of board.html and run in node (skipped without node).
func TestPostJSONReportsEveryFailure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	fn := regexp.MustCompile(`(?s)\nasync function postJSON\(.*?\n}\n`).FindString(string(board))
	if fn == "" {
		t.Fatal("board.html has no postJSON")
	}
	script := fn + `
const reply = (status, body, ctype) => async () => ({ ok: status < 300, status, json: async () => JSON.parse(body) });
const cases = {
  notFound: reply(404, "no such task"),
  gateway: reply(502, "bad gateway"),
  rejected: async () => { throw new TypeError("Failed to fetch"); },
  serverError: reply(500, '{"ok":false,"error":"boom"}'),
  good: reply(200, '{"ok":true}'),
  badStatusNoOk: reply(500, "{}"),
};
(async () => {
  const out = {};
  for (const [k, f] of Object.entries(cases)) { globalThis.fetch = f; out[k] = await postJSON("/api/say?id=x", { body: "hi" }); }
  console.log(JSON.stringify(out));
})();
`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var out map[string]struct {
		OK    *bool
		Error string
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("node printed %q: %v", b, err)
	}
	for _, k := range []string{"notFound", "gateway", "rejected", "badStatusNoOk"} {
		if o := out[k]; o.OK == nil || *o.OK || o.Error == "" {
			t.Errorf("%s: want ok:false with an error, got %+v", k, o)
		}
	}
	if o := out["serverError"]; o.Error != "boom" {
		t.Errorf("serverError: %+v", o)
	}
	if o := out["good"]; o.OK == nil || !*o.OK {
		t.Errorf("good: %+v", o)
	}
}
