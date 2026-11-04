package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// «Check it yourself» on the card: its steps, each with a box of its own, from r.self (the Go SelfView), and its
// row in «What blocks the merge». The functions are cut out of board.html and run in node (skipped without it).
func TestBoardSelfCheckSteps(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(board)
	var parts []string
	for _, re := range []string{
		`(?m)^const plural = .*$`, `(?s)\nconst MR_ICON = \{.*?\n\};\n`, `(?m)^const CHK = .*$`, `(?m)^const stepHTML = .*$`,
		`(?s)\nfunction selfTitle\(.*?\n}\n`, `(?s)\nfunction selfBlock\(.*?\n}\n`, `(?s)\nfunction tryRow\(.*?\n}\n`, `(?s)\nfunction blockersHTML\(.*?\n}\n`, `(?m)^const mergeHeld = .*$`,
	} {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("board.html has no %s", re)
		}
		parts = append(parts, m)
	}
	script := `
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
let data = {};
const review = null, isAsked = () => false, mrName = (u) => u, reviewFixBtn = () => "", assessing = () => false;
` + strings.Join(parts, "\n") + `
const steps = [{ open: "/settings", do: "Save <it>", expect: "Saved" }, { do: "Open the card", done: true }, { do: "Merge", done: true, stale: 2 }];
const r = (self) => ({ id: "a#1", self });
const out = {
  open: selfBlock(r({ required: true, steps, left: 2, blocks: true })),
  done: selfBlock(r({ required: true, steps: steps.map((s) => ({ ...s, done: true, stale: 0 })), left: 0, done: true })),
  one: selfBlock(r({ steps: [{ do: "Look", done: true, stale: 3 }], left: 1 })),
  skipped: selfBlock(r({ required: true, steps, left: 2, skipped: "docs only" })),
  none: selfBlock(r(null)),
  blockers: blockersHTML(r({ required: true, steps, left: 2, blocks: true })),
  noBlockers: blockersHTML(r({ required: true, steps, left: 0, done: true })),
  tryNone: selfBlock(r({ steps, left: 2, canTry: true })),
  tryStarting: selfBlock(r({ steps, left: 2, canTry: true, try: { state: "starting" } })),
  tryRunning: selfBlock(r({ steps, left: 2, canTry: true, tried: true, try: { state: "running", url: "http://127.0.0.1:5000", fresh: true } })),
  tryOld: selfBlock(r({ steps, left: 2, canTry: true, try: { state: "running", url: "http://127.0.0.1:5000" } })),
  tryFailed: selfBlock(r({ steps, left: 2, canTry: true, try: { state: "failed", error: "exit 3 <boom>" } })),
  tryPhone: (data = { remote: true }, selfBlock(r({ steps, left: 2, canTry: true }))),
  held: String(mergeHeld(r({ blocks: true }))),
  notHeld: String(mergeHeld(r({ blocks: false }))),
};
console.log(JSON.stringify(out));
`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("node printed %q: %v", b, err)
	}
	has := func(name string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(out[name], want) {
				t.Errorf("%s: no %q in %s", name, want, out[name])
			}
		}
	}
	lacks := func(name string, bads ...string) {
		t.Helper()
		for _, bad := range bads {
			if strings.Contains(out[name], bad) {
				t.Errorf("%s: unexpected %q in %s", name, bad, out[name])
			}
		}
	}
	// A box per step: the page, the step escaped, what to see; a stale tick is a box to tick again.
	has("open", "Check it yourself: 2 of 3 left", "The merge waits for it", "<code>/settings</code> · Save &lt;it&gt;", "→ Saved",
		`data-step="0" data-on="1"`, `data-step="1" data-on="0"`, `data-step="2" data-on="1"`, "checked before 2 new commits")
	if n := strings.Count(out["open"], `class="chk"`); n != 3 {
		t.Errorf("open: %d boxes, want 3", n)
	}
	has("done", "You checked it", `mr-advice self done`)
	lacks("done", "The merge waits", "checked before")
	has("one", "You checked it before 3 new commits")
	has("skipped", "Check skipped", "Skipped: docs only")
	if out["none"] != "" {
		t.Errorf("no check: %q", out["none"])
	}
	// «Try this build»: to start, starting, running at the branch as it is (Open, Stop), at older commits, failed.
	lacks("open", "data-try")
	has("tryNone", `data-try`, "Try this build", `class="primary"`)
	has("tryStarting", "Building and starting…", "data-trystop")
	lacks("tryStarting", `data-try "`, `data-try>`)
	has("tryRunning", ">Open<", "data-trystop", "http://127.0.0.1:5000")
	has("tryOld", "Try the new commits", "data-trystop")
	has("tryFailed", "Try again", "exit 3 &lt;boom&gt;")
	lacks("tryPhone", "data-try")
	has("blockers", "What blocks the merge", "Check it yourself: 2 of 3 steps left", "data-selfskip")
	lacks("noBlockers", "Check it yourself")
	if out["held"] != "true" || out["notHeld"] != "false" {
		t.Errorf("mergeHeld: %s / %s", out["held"], out["notHeld"])
	}
}

// The settings have «Check it yourself» and a project's «Try this build» command.
func TestSettingsSelfCheck(t *testing.T) {
	for _, want := range []string{`select(cfg, "selfCheck"`, `{ def: "advised"`, `pp.try = tryIn.value.trim()`} {
		if !strings.Contains(string(settingsPage), want) {
			t.Errorf("settings.html lacks %q", want)
		}
	}
}
