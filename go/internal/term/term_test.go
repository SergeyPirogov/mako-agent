package term

import (
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSession(t *testing.T) {
	changes := make(chan struct{}, 10)
	s, err := Start(Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh", Token: "tok",
		Args:    []string{"-c", `printf 'привіт\n'; read x; echo "got $x"`},
		OnState: func() { changes <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	past, events := waitFor(t, s, "привіт")
	if events == nil || !strings.Contains(past, "привіт") {
		t.Fatalf("past %q", past)
	}
	s.Input("yes\r")
	got := past
	for e := range events {
		got += e.D
		if e.T == "x" {
			break
		}
	}
	if !strings.Contains(got, "got yes") {
		t.Fatalf("output %q", got)
	}
	<-changes
	if s.Running() || s.State().State != "exited" {
		t.Fatalf("state %+v", s.State())
	}
}

func waitFor(t *testing.T, s *Session, text string) (string, chan Event) {
	for i := 0; i < 100; i++ {
		past, events := s.Subscribe()
		if strings.Contains(past, text) {
			return past, events
		}
		if events != nil {
			s.Unsubscribe(events)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q", text)
	return "", nil
}

func TestHooks(t *testing.T) {
	s := &Session{state: "working", subs: map[chan Event]struct{}{}}
	steps := []struct{ body, state, reason, tool string }{
		{`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm test\nmore"}}`, "working", "", "Bash: npm test"},
		{`{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`, "waiting", "permission for Bash", "Bash: npm test"},
		{`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`, "working", "", ""},
		{`{"hook_event_name":"Stop"}`, "done", "", ""},
	}
	for _, st := range steps {
		if err := s.Hook([]byte(st.body)); err != nil {
			t.Fatal(err)
		}
		if got := s.State(); got.State != st.state || got.Reason != st.reason || got.Tool != st.tool {
			t.Fatalf("%s: %+v", st.body, got)
		}
	}
}

// The hooks name Claude Code's session: told once, and again when it is a new one.
func TestHookSession(t *testing.T) {
	var named []string
	s := &Session{state: "working", subs: map[chan Event]struct{}{}, onSess: func(id string) { named = append(named, id) }}
	for _, b := range []string{
		`{"hook_event_name":"UserPromptSubmit","session_id":"a1"}`,
		`{"hook_event_name":"PreToolUse","session_id":"a1","tool_name":"Read","tool_input":{}}`,
		`{"hook_event_name":"Stop"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"b2"}`,
	} {
		if err := s.Hook([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(named, ",") != "a1,b2" || s.SessionID() != "b2" {
		t.Fatalf("named %v, now %q", named, s.SessionID())
	}
}

func TestValidPrefix(t *testing.T) {
	b := []byte("ab✓")
	if n := validPrefix(b); n != len(b) {
		t.Fatal(n)
	}
	if n := validPrefix(b[:len(b)-1]); n != 2 {
		t.Fatal(n)
	}
}

func TestPlainTail(t *testing.T) {
	if got := PlainTail("\x1b[1;32mDone\x1b[0m\r\n\x1b]0;title\x07ok", 100); got != "Done\nok" {
		t.Fatalf("%q", got)
	}
}

// An agent that ignores the hang-up, with a descendant in a process group of its own that holds the terminal,
// leaves the live ones after Stop.
func TestStopStubbornAgent(t *testing.T) {
	defer func(g time.Duration) { stopGrace = g }(stopGrace)
	stopGrace = 200 * time.Millisecond
	s, err := Start(Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh",
		Args: []string{"-c", `trap "" HUP; set -m; sleep 30 & echo "pid=$!"; sleep 30`}})
	if err != nil {
		t.Fatal(err)
	}
	past, events := waitFor(t, s, "pid=")
	if events != nil {
		s.Unsubscribe(events)
	}
	m := regexp.MustCompile(`pid=(\d+)`).FindStringSubmatch(past)
	for m == nil {
		time.Sleep(20 * time.Millisecond)
		m = regexp.MustCompile(`pid=(\d+)`).FindStringSubmatch(s.Output(1000))
	}
	pid, _ := strconv.Atoi(m[1])
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	mgr := NewManager(t.TempDir())
	mgr.Put(s)
	if mgr.Live() != 1 {
		t.Fatal("not live")
	}
	s.Stop()
	for end := time.Now().Add(2 * time.Second); mgr.Live() != 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("still live after Stop: %+v", s.State())
		}
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the descendant is gone: the test checks nothing")
	}
	if s.State().State != "exited" {
		t.Fatalf("state %+v", s.State())
	}
}
