package web

import (
	"os/exec"
	"strings"
	"testing"
)

// A permission request shows what it asks for whole (permArg), while a step's head keeps one short line (toolArg):
// the block that defines them runs here in Node.
func TestBoardPermissionShowsWholeCommand(t *testing.T) {
	src := string(board)
	from, to := strings.Index(src, "// toolArg:begin"), strings.Index(src, "// toolArg:end")
	if from < 0 || to < from {
		t.Fatal("board.html has no toolArg:begin … toolArg:end block")
	}
	if strings.Contains(src, "toolArg(it.input).slice(2) || JSON.stringify") {
		t.Error("a permission view still cuts the request with toolArg")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := src[from:to] + `
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
const long = "cd /Users/x/.worktrees/mako-agent/mako-agent-499 && git push origin feature/499-trekery-retry-z-backoff-na-429-5xx-kesh 2>&1 | tail -1 && gh pr create --fill --base main --head feature/499-trekery-retry";
eq(permArg({ command: long }), long, "a long command, whole");
eq(permArg({ command: "git add -A\ngit commit -m x" }), "git add -A\ngit commit -m x", "every line");
eq(permArg({ file_path: "/a/b.go", content: "x" }), "/a/b.go", "a file");
eq(permArg({ foo: 1 }), '{"foo":1}', "no known field: the input as JSON");
eq(permArg(undefined), "{}", "no input");
eq(toolArg({ command: long }), ": " + long.slice(0, 160), "a step's head stays short");
eq(toolArg({ command: "a\nb" }), ": a", "a step's head: the first line");
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A request a hook asks about says why, escaped, in both of its views (the dock and the conversation); one without
// a reason adds nothing.
func TestBoardPermissionWhy(t *testing.T) {
	src := string(board)
	if n := strings.Count(src, "${permWhy(it)}"); n != 2 {
		t.Fatalf("permWhy in %d permission views, want 2", n)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	line := func(prefix string) string {
		i := strings.Index(src, prefix)
		if i < 0 {
			t.Fatalf("board.html has no %q", prefix)
		}
		return src[i : i+strings.Index(src[i:], "\n")]
	}
	script := line("const esc = ") + "\n" + line("const permWhy = ") + `
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
eq(permWhy({ reason: "Mako asks <you>" }), '<div class="perm-why">Mako asks &lt;you&gt;</div>', "a reason");
eq(permWhy({}), "", "no reason");
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
