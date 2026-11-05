package chat

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// fixtureLines: testdata/background_agents.jsonl, a session that launches three subagents in the background, ends
// its turn and hears from them afterwards (one by a system event, one by a user message, one by a transcript's
// queued command, which then repeats). The launch result is the one Claude Code prints; the events follow its
// stream-json schema (system task_started / task_notification).
func fixtureLines(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/background_agents.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, append([]byte{}, sc.Bytes()...))
	}
	return out
}

func newBare() *Session {
	return &Session{ID: "x", state: "working", pending: map[string]json.RawMessage{}, lineMax: maxLine}
}

func statuses(s *Session) string {
	var out []string
	for _, it := range s.Snapshot().Items {
		if it.Kind == "tool" {
			out = append(out, it.Status)
		}
	}
	return strings.Join(out, " ")
}

func TestBackgroundSubagents(t *testing.T) {
	s := newBare()
	lines := fixtureLines(t)
	feed := func(from, to int) {
		for _, l := range lines[from:to] {
			s.handle(l)
		}
	}

	feed(0, 10) // up to the end of the turn
	snap := s.Snapshot()
	if got := statuses(s); got != "background background background" {
		t.Fatalf("after the launch: %s", got)
	}
	if snap.Background != 3 {
		t.Fatalf("background after the result = %d, want 3", snap.Background)
	}
	if snap.State != "done" {
		t.Errorf("state %q: the agent waits, so the turn is over", snap.State)
	}
	if it := snap.Items[1]; it.TaskID != "aaa111" || it.Status != "background" || !strings.HasPrefix(it.Output, "Async agent launched") {
		t.Errorf("launch: %+v", it)
	}

	feed(10, 11) // a system task_notification
	snap = s.Snapshot()
	if got := statuses(s); got != "ok background background" || snap.Background != 2 {
		t.Fatalf("after the first report: %s, %d in the background", got, snap.Background)
	}
	if it := snap.Items[1]; it.Output != `Agent "Explore part 1" finished` || it.End == 0 {
		t.Errorf("report: %+v", it)
	}

	feed(11, 12) // a user message with <task-notification>: a failed one
	snap = s.Snapshot()
	if got := statuses(s); got != "ok error background" || snap.Background != 1 {
		t.Fatalf("after the failure: %s, %d", got, snap.Background)
	}
	if out := snap.Items[2].Output; !strings.Contains(out, "failed") || !strings.Contains(out, "boom") {
		t.Errorf("a failed subagent says why: %q", out)
	}

	feed(12, 13) // a transcript's queued command
	snap = s.Snapshot()
	if got := statuses(s); got != "ok error ok" || snap.Background != 0 {
		t.Fatalf("after the last report: %s, %d", got, snap.Background)
	}
	if out := snap.Items[3].Output; !strings.Contains(out, "Findings: all good") {
		t.Errorf("the subagent's report is the output: %q", out)
	}

	v := s.version
	feed(13, 14) // the same task notifies again: nothing changes
	if got := statuses(s); got != "ok error ok" || s.version != v || s.Snapshot().Items[3].Output != snap.Items[3].Output {
		t.Errorf("a repeated notification changed the step: %s", got)
	}
}

// The step is found by its task when the notification does not name the tool call.
func TestBackgroundEndedByTask(t *testing.T) {
	s := newBare()
	lines := fixtureLines(t)
	for _, l := range lines[:10] {
		s.handle(l)
	}
	s.handle([]byte(`{"type":"system","subtype":"task_notification","task_id":"bbb222","status":"stopped","summary":"stopped by user"}`))
	if got := statuses(s); got != "background error background" {
		t.Fatalf("%s", got)
	}
	s.handle([]byte(`{"type":"system","subtype":"task_notification","task_id":"nope","tool_use_id":"nope","status":"completed","summary":"x"}`))
	s.handle([]byte(`{"type":"system","subtype":"task_notification","status":"completed","summary":"x"}`))
	if s.Snapshot().Background != 2 {
		t.Errorf("a notification of an unknown task ended a step: %s", statuses(s))
	}
}

// A subagent that was not asked to run in the background ends with its result, as before.
func TestForegroundSubagentEnds(t *testing.T) {
	s := newBare()
	s.handle([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"f1","name":"Task","input":{"description":"d","subagent_type":"Explore"}}]},"parent_tool_use_id":null}`))
	if got := statuses(s); got != "running" {
		t.Fatalf("%s", got)
	}
	s.handle([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"f1","content":"report"}]},"parent_tool_use_id":null}`))
	if got, n := statuses(s), s.Snapshot().Background; got != "ok" || n != 0 {
		t.Errorf("%s, %d in the background", got, n)
	}
}

// A launch that failed (the tool errored) is an error, not a subagent in the background.
func TestBackgroundLaunchFailed(t *testing.T) {
	s := newBare()
	s.handle([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"f1","name":"Agent","input":{"description":"d","run_in_background":true}}]},"parent_tool_use_id":null}`))
	s.handle([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"f1","content":"no such agent","is_error":true}]},"parent_tool_use_id":null}`))
	if got := statuses(s); got != "error" {
		t.Errorf("%s", got)
	}
}

// A report that comes before the launch result ends the step for good.
func TestBackgroundReportFirst(t *testing.T) {
	s := newBare()
	lines := fixtureLines(t)
	s.handle(lines[1])
	s.handle([]byte(`{"type":"system","subtype":"task_notification","task_id":"aaa111","tool_use_id":"toolu_A","status":"completed","summary":"done"}`))
	s.handle(lines[3]) // the launch result, late
	if got := statuses(s); got != "ok running running" {
		t.Errorf("%s", got)
	}
	if it := s.Snapshot().Items[1]; it.Output != "done" || it.TaskID != "aaa111" || it.End == 0 {
		t.Errorf("the late launch result overwrote the report: %+v", it)
	}

	// The same for a report that said the task failed: it stays an error.
	s = newBare()
	s.handle(lines[1])
	s.handle([]byte(`{"type":"system","subtype":"task_notification","task_id":"aaa111","tool_use_id":"toolu_A","status":"failed","summary":"boom"}`))
	s.handle(lines[3])
	if it := s.Snapshot().Items[1]; it.Status != "error" || it.Output != "boom" {
		t.Errorf("%+v", it)
	}
}

// Only «completed» is a success; any other status, also one Claude Code adds later, is not.
func TestBackgroundStatuses(t *testing.T) {
	lines := fixtureLines(t)
	for status, want := range map[string]string{"completed": "ok", "": "ok", "failed": "error", "stopped": "error", "killed": "error", "cancelled": "error"} {
		s := newBare()
		for _, l := range lines[:10] {
			s.handle(l)
		}
		s.handle([]byte(`{"type":"system","subtype":"task_notification","task_id":"aaa111","tool_use_id":"toolu_A","status":"` + status + `","summary":"s"}`))
		if got := s.Snapshot().Items[1].Status; got != want {
			t.Errorf("status %q: step is %q, want %q", status, got, want)
		}
	}
}

// A closed agent would take its subagents with it: it stays open while any is out, and its countdown starts
// again when the last one reports.
func TestBackgroundKeepsAgentAwake(t *testing.T) {
	s := newBare()
	lines := fixtureLines(t)
	for _, l := range lines[:10] {
		s.handle(l)
	}
	s.sleep()
	if s.asleep || s.stopping {
		t.Fatal("the agent was closed with subagents out")
	}
	for _, l := range lines[10:13] {
		s.handle(l)
	}
	s.mu.Lock()
	armed := s.idle != nil || s.idleAfter == 0
	s.mu.Unlock()
	if !armed {
		t.Error("the countdown did not start once the last subagent reported")
	}
	s.sleep()
	if !s.asleep {
		t.Error("an agent with nobody out and nothing to do should go to sleep")
	}
}

// A report that never comes does not keep the agent open for ever: past BackgroundMax it sleeps, and until then the
// countdown keeps running, so the wait is checked again.
func TestBackgroundWaitIsBounded(t *testing.T) {
	s := newBare()
	s.idleAfter = time.Hour // armed, never fired in the test
	lines := fixtureLines(t)
	for _, l := range lines[:10] {
		s.handle(l)
	}
	s.mu.Lock()
	s.idle = nil
	s.mu.Unlock()
	s.sleep()
	s.mu.Lock()
	rearmed := s.idle != nil
	s.mu.Unlock()
	if s.asleep || !rearmed {
		t.Fatalf("a fresh subagent: asleep %v, countdown running again %v", s.asleep, rearmed)
	}
	s.mu.Lock()
	for i := range s.items {
		if s.items[i].Status == "background" {
			s.items[i].At = time.Now().Add(-BackgroundMax - time.Minute).UnixMilli()
		}
	}
	s.mu.Unlock()
	s.sleep()
	if !s.asleep {
		t.Error("the agent still waits for reports that are past BackgroundMax")
	}
}

// Replaying a transcript: the reports in it end the steps; a subagent it never heard from was cut with the process.
func TestReplayBackground(t *testing.T) {
	dir := t.TempDir()
	lines := fixtureLines(t)
	write := func(name string, ls [][]byte) string {
		p := dir + "/" + name
		os.WriteFile(p, []byte(string(joinLines(ls))), 0o600)
		return p
	}
	snap := ReplayIn("x", "", write("all.jsonl", lines))
	if snap.Background != 0 || snap.Items[1].Status != "ok" || snap.Items[2].Status != "error" || snap.Items[3].Status != "ok" {
		t.Errorf("reported: %d in the background, %+v", snap.Background, snap.Items[1:4])
	}
	snap = ReplayIn("x", "", write("cut.jsonl", lines[:10]))
	if snap.Background != 0 {
		t.Errorf("a replay has no process: %d still in the background", snap.Background)
	}
	for _, it := range snap.Items[1:4] {
		if it.Status != "error" || !strings.Contains(it.Output, "Interrupted") {
			t.Errorf("not reported: %+v", it)
		}
	}
}

func joinLines(ls [][]byte) []byte {
	var out []byte
	for _, l := range ls {
		out = append(append(out, l...), '\n')
	}
	return out
}

func TestParseNotification(t *testing.T) {
	n, ok := parseNotification("<task-notification>\n<task-id>t1</task-id>\n<tool-use-id>u1</tool-use-id>\n<status>completed</status>\n<summary>S</summary>\n<result>R</result>\n</task-notification>")
	if !ok || n.taskID != "t1" || n.toolUseID != "u1" || n.status != "completed" || n.summary != "S\n\nR" {
		t.Errorf("%+v %v", n, ok)
	}
	for _, text := range []string{"hello", "<command-name>x</command-name>", "<task-notification></task-notification>"} {
		if _, ok := parseNotification(text); ok {
			t.Errorf("%q is not a notification", text)
		}
	}
}

// The step of a subagent in the background is its own block in the turn, not a step among the others.
func TestGroupBackgroundSubagent(t *testing.T) {
	items := []Item{{Kind: "user", Text: "go"}, tool("Agent", "background", map[string]any{"run_in_background": true}, "launched"), tool("Bash", "ok", map[string]any{"command": "ls"}, "")}
	turns := Group(items, "")
	if got := kinds(turns[1]); got != "item commands" {
		t.Errorf("blocks %q", got)
	}
}
