package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// The banner of a chat the Claude limit cut says when it goes on by itself, or that it does not for this chat, not
// «Your turn»; the switch stays while the limit is kept. The functions are cut out of board.html and run in node
// (skipped where node is not installed).
func TestLimitBanner(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(board)
	var parts []string
	for _, re := range []string{`(?m)^const limitCut = .*$`, `(?s)\nfunction limitText\(.*?\n}\n`, `(?s)\nfunction limitButtons\(.*?\n}\n`} {
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
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");
const fmtWhen = () => "13:10";
` + strings.Join(parts, "\n") + `
const ahead = Math.floor(Date.now() / 1000) + 3600, past = Math.floor(Date.now() / 1000) - 60;
const out = {
  auto: limitText({ state: "limited", limitedUntil: ahead, resumeOnReset: true }),
  off: limitText({ state: "limited", limitedUntil: ahead, resumeOnReset: false }),
  soon: limitText({ state: "limited", limitedUntil: past, resumeOnReset: true }),
  unknown: limitText({ state: "limited" }),
  cutAfterRestart: String(limitCut({ state: "limited", limitedUntil: past })),
  buttonsPast: limitButtons({ state: "limited", limitedUntil: past, resumeOnReset: true }),
  buttonsUnknown: limitButtons({ state: "limited" }),
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
	for name, want := range map[string]string{
		"auto":            "goes on by itself at 13:10",
		"off":             "until 13:10 · going on by itself is off for this chat",
		"soon":            "goes on by itself as soon as",
		"unknown":         "Claude limit reached",
		"cutAfterRestart": "true",
		"buttonsPast":     `data-limit-auto`,
	} {
		if !strings.Contains(out[name], want) {
			t.Errorf("%s: no %q in %q", name, want, out[name])
		}
	}
	if !strings.Contains(out["buttonsPast"], `aria-checked="true"`) || strings.Contains(out["buttonsUnknown"], "data-limit-auto") {
		t.Errorf("switch: %q / %q", out["buttonsPast"], out["buttonsUnknown"])
	}
}
