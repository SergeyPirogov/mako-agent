package web

import (
	"os/exec"
	"strings"
	"testing"
)

// A limit window whose reset time has passed is fresh again: the board shows it at 0%, not the percent the agents
// last reported before the reset.
func TestBoardLimitsAfterReset(t *testing.T) {
	src := string(board)
	from := strings.Index(src, "function limitsHTML(l) {")
	to := strings.Index(src[max(from, 0):], "\n}\n")
	if from < 0 || to < 0 {
		t.Fatal("board.html has no limitsHTML")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `const data = { limitPausePct: 85 };
const resetTime = () => "later";
` + src[from:from+to+3] + `
const now = Math.floor(Date.now() / 1000);
const pcts = html => [...html.matchAll(/<b>(\d+)%<\/b>/g)].map(m => +m[1]).join(",");
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
eq(pcts(limitsHTML({ fiveHour: { used: 0.31, resetsAt: now + 3600 }, week: { used: 0.93, resetsAt: now + 86400 } })), "31,93", "before the reset");
eq(pcts(limitsHTML({ fiveHour: { used: 0.31, resetsAt: now - 60 }, week: { used: 0.93, resetsAt: now + 86400 } })), "0,93", "5 h reset");
eq(pcts(limitsHTML({ fiveHour: { used: 0.9, resetsAt: now - 60 }, week: { used: 0.93, resetsAt: now - 60 } })), "0,0", "both reset");
eq(limitsHTML({ fiveHour: { used: 0.9, resetsAt: now - 60 } }).includes("bad"), false, "a reset window is not red");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}
