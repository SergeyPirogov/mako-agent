package chat

import (
	"encoding/json"
	"math"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestTestCounts(t *testing.T) {
	for _, c := range []struct {
		name, cmd, out string
		passed, failed int
		ok             bool
	}{
		{"go -v", "go test -v ./internal/core", "=== RUN   TestA\n--- PASS: TestA (0.00s)\n--- PASS: TestB (0.00s)\n    --- PASS: TestB/sub (0.00s)\n--- FAIL: TestC (0.00s)\nFAIL\nFAIL\tmako/internal/core\t0.418s\n", 3, 1, true},
		{"go packages", "go test ./...", "ok  \tmako/internal/chat\t6.7s\nok  \tmako/internal/web\t1.2s\n?   \tmako/cmd\t[no test files]\n", 2, 0, true},
		{"go package failed", "cd /w && go test ./...", "ok  \tmako/a\t0.1s\n--- FAIL: TestX (0.00s)\n    x_test.go:12: bad\nFAIL\nFAIL\tmako/b\t0.2s\nFAIL\n", 0, 1, true},
		{"gotestsum", "gotestsum ./...", "DONE 48 tests, 1 failure in 3.2s\n", 47, 1, true},
		{"jest", "npm test", "Tests:       1 failed, 2 skipped, 47 passed, 50 total\nSnapshots:   0 total\n", 47, 1, true},
		{"jest ok", "npx jest src", "Tests:       12 passed, 12 total\n", 12, 0, true},
		{"vitest", "pnpm vitest run", " Test Files  1 failed | 4 passed (5)\n      Tests  1 failed | 47 passed (48)\n", 47, 1, true},
		{"vitest ansi", "npm run test:unit", "\x1b[2m      Tests \x1b[22m \x1b[1m\x1b[32m48 passed\x1b[39m\x1b[22m\x1b[90m (48)\x1b[39m\n", 48, 0, true},
		{"pytest", "pytest -q", "....F\n=========== 1 failed, 47 passed in 1.23s ===========\n", 47, 1, true},
		{"pytest errors", "python -m pytest", "=== 2 passed, 1 error in 0.5s ===\n", 2, 1, true},
		{"not tests", "ls -la", "--- PASS: x\n", 0, 0, false},
		{"silent", "go test ./...", "", 0, 0, false},
	} {
		p, f, ok := TestCounts(c.cmd, c.out)
		if p != c.passed || f != c.failed || ok != c.ok {
			t.Errorf("%s: got %d passed %d failed %v, want %d %d %v", c.name, p, f, ok, c.passed, c.failed, c.ok)
		}
	}
}

func TestCommitHash(t *testing.T) {
	for cmd, want := range map[string]string{
		`git commit -m "Stats: filter by project"`: "a91c0e2",
		`git add -A && git commit -m x`:            "a91c0e2",
		`git -C /w commit -m x`:                    "a91c0e2",
		`git log -1`:                               "",
	} {
		if got := CommitHash(cmd, "[feature/280 a91c0e2] Stats: filter by project\n 2 files changed\n"); got != want {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
	if got := CommitHash("git commit -m x", "[main (root-commit) 1234abc] x\n"); got != "1234abc" {
		t.Errorf("root commit: %q", got)
	}
	if got := CommitHash("git commit -m x", "nothing to commit, working tree clean\n"); got != "" {
		t.Errorf("nothing committed: %q", got)
	}
}

func TestStripCd(t *testing.T) {
	wt := "/Users/me/.worktrees/mako/mako-290"
	for in, want := range map[string]string{
		"cd " + wt + " && go test ./...":        "go test ./...",
		`cd "` + wt + `/go" && go vet ./...`:    "go vet ./...",
		"cd " + wt + "/go && cd internal && ls": "ls",
		"cd /tmp && ls":                         "cd /tmp && ls",
		"go test ./...":                         "go test ./...",
		"cd go && go build":                     "go build",
	} {
		if got := StripCd(in, wt); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := StripCd("cd /anywhere && make", ""); got != "make" {
		t.Errorf("unknown worktree: %q", got)
	}
}

func tool(name, status string, input any, output string) Item {
	b, _ := json.Marshal(input)
	return Item{Kind: "tool", ID: name + output, Tool: name, Input: b, Status: status, Output: output}
}

func kinds(t Turn) string {
	var k []string
	for _, b := range t.Blocks {
		k = append(k, b.Kind)
	}
	return strings.Join(k, " ")
}

func TestGroup(t *testing.T) {
	wt := "/w"
	items := []Item{
		{Kind: "user", Text: "Continue", At: 1000},
		{Kind: "text", Text: "Backend is done.", At: 2000},
		tool("Bash", "ok", map[string]string{"command": "cd /w && go test ./internal/core"}, "ok  \tmako/core\t0.4s\n"),
		tool("Bash", "ok", map[string]string{"command": `git commit -m "x"`}, "[f a91c0e2] x\n"),
		tool("TodoWrite", "ok", map[string]any{"todos": []map[string]string{{"content": "Backend", "status": "completed"}, {"content": "Front", "activeForm": "Writing the front", "status": "in_progress"}, {"content": "Tests", "status": "pending"}}}, ""),
		tool("Read", "ok", map[string]string{"file_path": "/w/a.go"}, ""),
		tool("Read", "ok", map[string]string{"file_path": "/w/b.go"}, ""),
		{Kind: "text", Text: "Writing the front."},
		tool("Edit", "ok", map[string]string{"file_path": "/w/web/board.html", "old_string": "a\nb", "new_string": "a\nb\nc"}, ""),
		tool("Bash", "error", map[string]string{"command": "go test ./..."}, "--- FAIL: TestX (0.00s)\nFAIL\n"),
		tool("Edit", "running", map[string]string{"file_path": "/w/web/style.css"}, ""),
	}
	turns := Group(items, wt)
	if len(turns) != 2 || turns[0].Who != "user" || turns[0].Text != "Continue" || turns[1].Who != "agent" || turns[1].At != 2000 {
		t.Fatalf("turns: %+v", turns)
	}
	a := turns[1]
	if got := kinds(a); got != "item commands reads item edits commands current" {
		t.Fatalf("blocks: %s", got)
	}
	cmds := a.Blocks[1]
	want := []Row{{I: 2, Label: "go test ./internal/core", Test: true, Passed: 1}, {I: 3, Label: `git commit -m "x"`, Commit: "a91c0e2"}}
	if !reflect.DeepEqual(cmds.Rows, want) {
		t.Errorf("commands: %+v", cmds.Rows)
	}
	if r := a.Blocks[2].Rows; len(r) != 2 || r[0].Label != "a.go" || r[1].Label != "b.go" {
		t.Errorf("reads: %+v", r)
	}
	if r := a.Blocks[3+1].Rows[0]; r.Label != "web/board.html" || r.Add != 3 || r.Del != 2 {
		t.Errorf("edit: %+v", r)
	}
	if b := a.Blocks[5]; b.Failed != 1 || !b.Rows[0].Test || b.Rows[0].Failed != 1 {
		t.Errorf("failed: %+v", b)
	}
	if b := a.Blocks[6]; !b.Running || b.Item != 10 || b.Rows[0].Label != "web/style.css" {
		t.Errorf("current: %+v", b)
	}
	p := ProgressOf(items)
	if p == nil || p.Done != 1 || p.Total != 3 || p.Current != "Writing the front" {
		t.Errorf("progress: %+v", p)
	}
	if st := statOf(items); st.Since != 1000 || st.Steps != 8 {
		t.Errorf("turn: %+v", st)
	}
}

func TestGroupMixedSteps(t *testing.T) {
	items := []Item{
		tool("Read", "ok", map[string]string{"file_path": "/w/a"}, ""),
		tool("Grep", "ok", map[string]string{"pattern": "x"}, ""),
		tool("Bash", "ok", map[string]string{"command": "ls"}, ""),
		tool("Read", "ok", map[string]string{"file_path": "/w/b"}, ""),
	}
	turns := Group(items, "/w")
	if len(turns) != 1 || kinds(turns[0]) != "steps" || len(turns[0].Blocks[0].Rows) != 4 {
		t.Fatalf("mixed: %+v", turns)
	}
	// An edit among them is never folded away.
	items = append(items, tool("Write", "ok", map[string]string{"file_path": "/w/c", "content": "x"}, ""))
	if got := kinds(Group(items, "/w")[0]); got != "reads search commands reads edits" {
		t.Errorf("with an edit: %s", got)
	}
}

func TestGroupPromptAndImages(t *testing.T) {
	items := []Item{
		{Kind: "user", Text: strings.Repeat("task ", 100)},
		{Kind: "text", Text: "ok"},
		{Kind: "user", Text: "Look at this\n" + ImageLine("/state/screenshots/a/1.png")},
	}
	turns := Group(items, "")
	if !turns[0].Prompt || turns[2].Prompt {
		t.Errorf("prompt: %+v", turns)
	}
	if turns[2].Text != "Look at this" || !reflect.DeepEqual(turns[2].Images, []string{"/state/screenshots/a/1.png"}) {
		t.Errorf("images: %+v", turns[2])
	}
}

// A permission given does not split the steps around it: in «Ask» mode one comes before every command.
func TestGroupSkipsPermissionsGiven(t *testing.T) {
	items := []Item{
		{Kind: "permission", ID: "r1", Tool: "Bash", Status: "allowed"},
		tool("Bash", "ok", map[string]string{"command": "go vet ./..."}, ""),
		{Kind: "permission", ID: "r2", Tool: "Bash", Status: "always"},
		tool("Bash", "ok", map[string]string{"command": "go test ./..."}, ""),
		{Kind: "permission", ID: "r3", Tool: "Bash", Status: "denied"},
	}
	turns := Group(items, "")
	if got := kinds(turns[0]); got != "commands item" || len(turns[0].Blocks[0].Rows) != 2 {
		t.Fatalf("blocks: %s %+v", got, turns[0].Blocks)
	}
}

// The cost of a turn going on: its tokens at the price of the turns before, of this process or of one before it
// (each «Continue» starts a new one); the turn's result then says it exactly.
func TestTurnCostEstimate(t *testing.T) {
	s := &Session{state: "working", pending: map[string]json.RawMessage{}, cmd: &exec.Cmd{}}
	call := func(id string, n int) {
		s.handle([]byte(`{"type":"assistant","message":{"id":"` + id + `","usage":{"input_tokens":` + strconv.Itoa(n) + `},"content":[]},"parent_tool_use_id":null}`))
	}
	call("m1", 1000)
	if c := s.Snapshot().Turn; c.Cost != 0 {
		t.Fatalf("no price known yet, yet %+v", c)
	}
	s.handle([]byte(`{"type":"result","total_cost_usd":0.5}`))
	if c := s.Snapshot().Turn; c.Cost != 0.5 || c.Estimate {
		t.Fatalf("finished turn: %+v", c)
	}
	// A new process continues the conversation: its cost starts from 0, the price is still known.
	s.mu.Lock()
	s.cmd, s.spent, s.spentTok, s.procTok, s.state = &exec.Cmd{}, 0, 0, 0, "working"
	s.mu.Unlock()
	call("m2", 400)
	if c := s.Snapshot().Turn; !c.Estimate || math.Abs(c.Cost-0.2) > 1e-9 {
		t.Fatalf("estimate: %+v", c)
	}
	s.handle([]byte(`{"type":"result","total_cost_usd":0.25}`))
	if c := s.Snapshot().Turn; c.Cost != 0.25 || c.Estimate {
		t.Fatalf("exact after the result: %+v", c)
	}
}

// A finished turn sums up: its time and cost from the result, the files it edited (each once) with their lines,
// its last test run, its commits. The turn going on has none yet.
func TestTurnSummary(t *testing.T) {
	items := []Item{
		{Kind: "user", Text: "Go", At: 1000},
		tool("Edit", "ok", map[string]string{"file_path": "/w/a.go", "old_string": "a", "new_string": "a\nb"}, ""),
		tool("Bash", "error", map[string]string{"command": "go test ./..."}, "--- FAIL: TestX (0.00s)\n--- PASS: TestY (0.00s)\n"),
		tool("Edit", "ok", map[string]string{"file_path": "/w/a.go", "old_string": "x", "new_string": "y"}, ""),
		tool("Write", "ok", map[string]string{"file_path": "/w/b.go", "content": "p\nq\nr"}, ""),
		{Kind: "text", Text: "Fixed."},
		tool("Bash", "ok", map[string]string{"command": "go test ./..."}, "--- PASS: TestX (0.00s)\n--- PASS: TestY (0.00s)\n"),
		tool("Bash", "ok", map[string]string{"command": `git commit -m "x"`}, "[f a91c0e2] x\n"),
		{Kind: "result", Seconds: 65, Cost: 0.84},
		{Kind: "user", Text: "More"},
		tool("Bash", "running", map[string]string{"command": "make"}, ""),
	}
	turns := Group(items, "/w")
	want := &Summary{Seconds: 65, Cost: 0.84, Files: 2, Add: 6, Del: 2, Tests: &Tests{Passed: 2}, Commits: 1}
	if got := turns[1].Summary; !reflect.DeepEqual(got, want) {
		t.Errorf("summary: %+v (tests %+v)", got, got.Tests)
	}
	if turns[3].Summary != nil {
		t.Errorf("the turn going on: %+v", turns[3].Summary)
	}
}

// A replayed conversation has no results: a turn your next message follows is over, and so is the last one once
// nobody runs the conversation; the time runs from your message to its last step.
func TestTurnSummaryWithoutResult(t *testing.T) {
	read := tool("Read", "ok", map[string]string{"file_path": "/w/a.go"}, "")
	read.At, read.End = 2000, 31000
	items := []Item{{Kind: "user", Text: "Look", At: 1000}, read, {Kind: "user", Text: "And now", At: 40000}, {Kind: "text", Text: "Done.", At: 41000}}
	turns := Group(items, "/w")
	if s := turns[1].Summary; s == nil || s.Seconds != 30 || s.Files != 0 || s.Tests != nil {
		t.Errorf("ended by your message: %+v", s)
	}
	if turns[3].Summary != nil {
		t.Errorf("the last turn of a live conversation: %+v", turns[3].Summary)
	}
	snap := Drawn(Snapshot{Items: items}, "/w")
	if s := snap.Turns[3].Summary; s == nil || s.Seconds != 1 {
		t.Errorf("the last turn of a conversation nobody runs: %+v", s)
	}
}

// The result of a turn says what this turn cost, not the process's total; a compaction drops the context until the
// next request says it.
func TestTurnCostAndCompact(t *testing.T) {
	s := &Session{state: "working", pending: map[string]json.RawMessage{}, cmd: &exec.Cmd{}}
	s.handle([]byte(`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":1000},"content":[]},"parent_tool_use_id":null}`))
	s.handle([]byte(`{"type":"result","total_cost_usd":0.5}`))
	s.handle([]byte(`{"type":"result","total_cost_usd":0.8}`))
	snap := s.Snapshot()
	if n := len(snap.Items); n != 2 || snap.Items[0].Cost != 0.5 || math.Abs(snap.Items[1].Cost-0.3) > 1e-9 {
		t.Fatalf("results: %+v", snap.Items)
	}
	if snap.Context != 1000 {
		t.Fatalf("context: %d", snap.Context)
	}
	s.handle([]byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"manual","pre_tokens":1000}}`))
	snap = s.Snapshot()
	if snap.Context != 0 || snap.Items[len(snap.Items)-1].Kind != "compact" {
		t.Fatalf("after compaction: context %d, items %+v", snap.Context, snap.Items)
	}
}
