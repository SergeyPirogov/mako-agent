package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The failed card's blocks are plain functions of board.html: they are cut out of the page and run in node
// (the test is skipped where node is not installed).
func TestFailedCardBlocks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(board)
	var parts []string
	for _, re := range []string{`(?m)^const checkMark = .*$`, `(?s)\nfunction checksHTML\(.*?\n}\n`, `(?s)\nfunction whyFailedHTML\(.*?\n}\n`} {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("board.html has no %s", re)
		}
		parts = append(parts, m)
	}
	script := `
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
const linkify = esc;
const card = (title, body, cls = "", note = "") => "<card " + title + "|" + cls + "|" + note + ">" + body + "</card>";
` + strings.Join(parts, "\n") + `
const results = [
  { name: "tests", command: "go test ./...", ok: false, seconds: 12, tail: "FAIL: TestX <boom>\nexit 1" },
  { name: "lint", command: "go vet ./...", ok: true, seconds: 0 },
  { name: "build", command: "go build ./...", ok: false, timedOut: true, seconds: 300 },
];
const out = {
  full: checksHTML({ checks: { state: "failed", fails: 3, commit: "0123456789abcdef", results } }, card, true),
  noChecks: checksHTML({}, card, true),
  emptyResults: checksHTML({ checks: { state: "failed", fails: 1, commit: "abc", results: [] } }, card, true),
  noResultsField: checksHTML({ checks: { state: "failed", fails: 1, commit: "abc" } }, card, true),
  withMR: checksHTML({ prUrl: "u", checks: { state: "failed", commit: "abc", results } }, card, true),
  doing: checksHTML({ checks: { state: "failed", fails: 2, commit: "0123456789abcdef", results } }, card),
  doingPassed: checksHTML({ checks: { state: "passed", commit: "abc", results } }, card),
  why: whyFailedHTML({ status: "failed", error: "checks failed 3 times:\ngo test ./... exit 1 <x>" }, true),
  whyNoJournal: whyFailedHTML({ status: "failed", error: "boom" }, false),
  cancelled: whyFailedHTML({ status: "cancelled", error: "stopped" }, true),
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
	has := func(name, want string) {
		t.Helper()
		if !strings.Contains(out[name], want) {
			t.Errorf("%s: no %q in %s", name, want, out[name])
		}
	}
	lacks := func(name, bad string) {
		t.Helper()
		if strings.Contains(out[name], bad) {
			t.Errorf("%s: unexpected %q in %s", name, bad, out[name])
		}
	}

	// Every command with its result and seconds, the tail of the failed ones escaped, a Retry button.
	for _, want := range []string{"go test ./...", "go vet ./...", "go build ./...", "failed · 12 s", "OK · &lt;1 s", "timed out · 300 s",
		"<pre class=\"code\">FAIL: TestX &lt;boom&gt;\nexit 1</pre>", `data-a="retry"`, "attempt 3", "01234567"} {
		has("full", want)
	}
	if n := strings.Count(out["full"], "<pre"); n != 1 {
		t.Errorf("full: %d tails, want 1 (only the command that printed one)", n)
	}
	for _, name := range []string{"noChecks", "emptyResults", "noResultsField", "withMR", "doingPassed"} {
		if out[name] != "" {
			t.Errorf("%s: want nothing, got %s", name, out[name])
		}
	}
	// The doing card keeps its short block: only what failed, no Retry.
	has("doing", "Checks before the MR failed")
	lacks("doing", "go vet")
	lacks("doing", "retry")

	// The red block: a heading, the error as it is (line breaks kept for the CSS), the journal link only with a journal.
	has("why", "Why it failed")
	has("why", "data-journal")
	has("why", "checks failed 3 times:\ngo test ./... exit 1 &lt;x&gt;")
	has("whyNoJournal", "Why it failed")
	lacks("whyNoJournal", "data-journal")
	lacks("cancelled", "Why it failed")
	has("cancelled", "stopped")
}
