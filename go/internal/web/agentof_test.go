package web

import (
	"os/exec"
	"strings"
	"testing"
)

// An agent that waits for you shows on the board in any column: a task in review whose agent asks for a
// permission is counted and marked, not left to be found in the chat (agentOf runs here in Node).
func TestBoardAgentWaitingOutOfWorkShows(t *testing.T) {
	src := string(board)
	from, to := strings.Index(src, "// agentOf:begin"), strings.Index(src, "// agentOf:end")
	if from < 0 || to < from {
		t.Fatal("board.html has no agentOf:begin … agentOf:end block")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `let data = { agents: { a: { state: "waiting" }, b: { state: "done" }, c: { state: "working" } } };
` + src[from:to] + `
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
eq(agentOf({ id: "a", column: "review" })?.state, "waiting", "in review, waiting for you: shown");
eq(agentOf({ id: "b", column: "review" }), undefined, "in review, done: the MR's state stays");
eq(agentOf({ id: "c", column: "done" }), undefined, "out of work, working: not shown");
eq(agentOf({ id: "b", column: "doing" })?.state, "done", "in work: always shown");
eq(agentOf({ id: "x", column: "doing" }), undefined, "no agent");
data = {};
eq(agentOf({ id: "a", column: "review" }), undefined, "no agents yet");
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
