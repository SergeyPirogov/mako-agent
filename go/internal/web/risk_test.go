package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// The change's risk on the MR card (r.risk, the Go RiskView): the level, what it does here and its signals with their
// files; its rows in «What blocks the merge» for a high risk held for the check and for protected paths; the Merge
// held by them; the stats card. The functions are cut out of board.html and run in node (skipped without it).
func TestBoardRisk(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(board)
	var parts []string
	for _, re := range []string{
		`(?m)^const plural = .*$`, `(?s)\nconst MR_ICON = \{.*?\n\};\n`, `(?m)^const sec = .*$`, `(?m)^const selfSkipBtn = .*$`,
		`(?m)^const riskName = .*$`, `(?m)^const RISK_ICON = .*$`, `(?s)\nfunction riskSection\(.*?\n}\n`,
		`(?s)\nfunction blockersHTML\(.*?\n}\n`, `(?m)^const mergeHeld = .*$`,
		`(?m)^const meter = .*$`, `(?m)^const scard = .*$`, `(?s)\nfunction riskCard\(.*?\n}\n`,
	} {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("board.html has no %s", re)
		}
		parts = append(parts, m)
	}
	words, _ := json.Marshal(i18n.WebStrings())
	script := `
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
const I18N = ` + string(words) + `;
function t(key, vars = {}) { return String(I18N[key] ?? key).replace(/\{(\w+)\}/g, (m, k) => (k in vars ? vars[k] : m)); }
const review = null, isAsked = () => false, mrName = (u) => u, reviewFixBtn = () => "", assessing = () => false;
` + strings.Join(parts, "\n") + `
const signals = [{ kind: "path", level: "high", text: "Touches migrations/**", files: ["migrations/002.sql"] }, { kind: "assessment", level: "low", text: "Agent's assessment: risk 2 of 5: <small>" }];
const block = { level: "block", policy: "block", signals: [{ kind: "path", level: "block", text: "Touches a protected path: .github/**", files: [".github/ci.yml"] }] };
const self = { required: true, steps: [{ do: "Look" }], left: 1, blocks: true };
const out = {
  high: riskSection({ risk: { level: "high", policy: "gate", signals } }),
  low: riskSection({ risk: { level: "low", policy: "auto", signals: [] } }),
  stale: riskSection({ risk: { level: "medium", policy: "advice", stale: true } }),
  none: riskSection({}),
  gate: blockersHTML({ id: "a#1", risk: { level: "high", policy: "gate" }, self }),
  always: blockersHTML({ id: "a#1", risk: { level: "medium", policy: "advice" }, self }),
  block: blockersHTML({ id: "a#1", risk: block }),
  held: String(mergeHeld({ risk: block })),
  notHeld: String(mergeHeld({ risk: { level: "low" } })),
  stats: riskCard({ merged: 6, low: 3, medium: 1, high: 2, block: 0, autoMerged: 2, autoMergedFixed: 1 }),
  statsNoAuto: riskCard({ merged: 1, low: 0, medium: 1, high: 0, block: 0, autoMerged: 0 }),
  statsNone: riskCard({ merged: 0 }),
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
	has("high", "Change risk", "High risk", "the merge waits for Check it yourself", "Touches migrations/**", "migrations/002.sql",
		`risk-l high`, `risk-l low`, "risk 2 of 5: &lt;small&gt;", "mr-advice risk high")
	has("low", "Low risk", "Merges by itself once CI is green and the AI review is clean")
	lacks("low", "risk-sigs")
	has("stale", "Medium risk", "The merge is yours", "read before the latest commits")
	if out["none"] != "" {
		t.Errorf("no risk, a section: %q", out["none"])
	}
	// The check holds the merge for the risk: the row says so, with its Skip; held by the setting, as before.
	has("gate", "What blocks the merge", "High change risk: check it yourself, 1 of 1 left", "data-selfskip")
	has("always", "Check it yourself: 1 of 1 step left")
	lacks("always", "High change risk")
	has("block", "What blocks the merge", "The change touches protected paths", ".github/ci.yml")
	if out["held"] != "true" || out["notHeld"] != "false" {
		t.Errorf("mergeHeld: %s / %s", out["held"], out["notHeld"])
	}
	has("stats", "Change risk", "6 merged", "Low risk", "Medium risk", "High risk", "Merged by itself", "1 of them needed a fix after the merge · 50%")
	lacks("stats", "Protected paths")
	has("statsNoAuto", "No merges by itself")
	if out["statsNone"] != "" {
		t.Errorf("no merges, a card: %q", out["statsNone"])
	}
}

// The settings: a project's risk row; its paths, a line each, «pattern level», a pattern without a known level high.
func TestSettingsRiskPaths(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(settingsPage)
	for _, want := range []string{`body.append(riskRow(tk, () => current))`, `data-rb="autoMergeLow"`, `data-rb="gateHigh"`, `data-rn`} {
		if !strings.Contains(src, want) {
			t.Errorf("settings.html lacks %q", want)
		}
	}
	var parts []string
	for _, re := range []string{`(?m)^const RISK_LEVELS = .*$`, `(?s)\nfunction riskPathsOf\(.*?\n}\n`} {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("settings.html has no %s", re)
		}
		parts = append(parts, m)
	}
	script := strings.Join(parts, "\n") + `
console.log(JSON.stringify(riskPathsOf("migrations/** high\n\n  .github/** BLOCK \n*.env*\ndocs/ medium\nmy dir/** low")));`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	want := `[{"pattern":"migrations/**","level":"high"},{"pattern":".github/**","level":"block"},{"pattern":"*.env*","level":"high"},{"pattern":"docs/","level":"medium"},{"pattern":"my dir/** low","level":"high"}]`
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("paths\n%s\nwant\n%s", got, want)
	}
}

// The board's "risk" is the view with the level and the policy, not the record's own reading of the diff under it.
func TestRecordViewRisk(t *testing.T) {
	v := recordView{Record: core.Record{ID: "a#1", Risk: &core.RiskLook{Commit: "abc"}}, Risk: &dispatch.RiskView{Level: core.RiskLow, Policy: "auto"}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"risk":{"level":"low","policy":"auto"}`) || strings.Contains(s, `"commit":"abc"`) {
		t.Fatalf("json %s", s)
	}
}
