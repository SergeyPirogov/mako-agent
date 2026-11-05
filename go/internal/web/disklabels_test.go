package web

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// Disk space: a merged task's worktree carries a «merged» mark, and the button that removes the merged ones says
// what it removes («Remove 5 merged worktrees · 22 MB»), not a bare «(5, 22 MB)». The functions are cut out of
// settings.html and run in node (skipped where node is not installed).
func TestDiskMergedMarkAndCleanLabel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := string(settingsPage)
	var parts []string
	for _, re := range []string{`(?m)^const pickKey = .*$`, `(?m)^const size = .*$`, `(?s)\nfunction cleanLabel\(.*?\n}\n`, `(?s)\nfunction worktreeRow\(.*?\n}\n`} {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("settings.html has no %s", re)
		}
		parts = append(parts, m)
	}
	words, _ := json.Marshal(i18n.WebStrings())
	script := `
const words = ` + string(words) + `;
const t = (k, v = {}) => (words[k] ?? k).replace(/\{(\w+)\}/g, (_, n) => v[n]);
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");
const picked = new Set(), diskAgo = () => "";
` + strings.Join(parts, "\n") + `
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
const mark = '<span class="badge ok">merged</span>';
const u = { repo: "mako-agent", base: "main" }, w = { path: "/wt/a", branch: "fix/a", bytes: 1 << 20, task: "a#1", hasRemote: true };
eq(worktreeRow(u, { ...w, reviewState: "merged", cleanable: true }).includes(mark), true, "merged task");
eq(worktreeRow(u, { ...w, reviewState: "opened" }).includes(mark), false, "open MR");
eq(worktreeRow(u, { ...w, task: "" }).includes(mark), false, "not a Mako task");
eq(cleanLabel(5, 22 << 20), "Remove 5 merged worktrees · 22 MB", "five");
eq(cleanLabel(1, 5 << 20), "Remove 1 merged worktree · 5 MB", "one");
eq(cleanLabel(0, 0), "Remove the merged ones", "none");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}
