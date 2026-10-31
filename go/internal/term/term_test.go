package term

import (
	"strings"
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
		{`{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`, "waiting", "дозвіл на Bash", ""},
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

func TestCleanEnv(t *testing.T) {
	got := cleanEnv([]string{"PATH=/bin", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDECODE=1", "CLAUDE_CONFIG_DIR=/c"})
	if strings.Join(got, " ") != "PATH=/bin CLAUDE_CONFIG_DIR=/c" {
		t.Fatal(got)
	}
}
