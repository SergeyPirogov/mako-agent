package chat

import (
	"strings"
	"sync"
	"testing"
)

// What claude -p --output-format stream-json prints when the login fails (seen with CLAUDE_CODE_OAUTH_TOKEN=invalid).
const (
	authAssistant = `{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"Failed to authenticate. API Error: 401 Invalid bearer token"}]},"parent_tool_use_id":null,"error":"authentication_failed","is_api_error_message":true,"api_error_status":401}`
	authResult    = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 Invalid bearer token","duration_ms":10}`
	okAssistant   = `{"type":"assistant","message":{"model":"claude-x","role":"assistant","content":[{"type":"text","text":"done"}]},"parent_tool_use_id":null}`
	okResult      = `{"type":"result","subtype":"success","is_error":false,"result":"done","duration_ms":10}`
)

// authCalls: what OnAuth was told, in order.
type authCalls struct {
	mu    sync.Mutex
	calls []string
}

func (a *authCalls) on(failed bool, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if failed {
		a.calls = append(a.calls, "failed:"+text)
	} else {
		a.calls = append(a.calls, "ok")
	}
}

func (a *authCalls) get() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.calls...)
}

func itemKinds(s *Session) map[string]int {
	n := map[string]int{}
	for _, it := range s.Snapshot().Items {
		n[it.Kind]++
	}
	return n
}

// A failed login shows once: one «auth» card, neither the agent's text nor an error, the state «auth», and Mako is
// told once. A message of yours goes on from it.
func TestAuthFailedTurn(t *testing.T) {
	var a authCalls
	s, in := limitSession(Options{OnAuth: a.on})
	defer s.Stop()
	s.handle([]byte(authAssistant))
	s.handle([]byte(authResult))
	n := itemKinds(s)
	if n["auth"] != 1 || n["text"] != 0 || n["error"] != 0 {
		t.Fatalf("items %v, want exactly one auth and no text or error", n)
	}
	snap := s.Snapshot()
	if snap.State != "auth" {
		t.Fatalf("state %q, want auth", snap.State)
	}
	for _, it := range snap.Items {
		if it.Kind == "auth" && !strings.Contains(it.Text, "Failed to authenticate") {
			t.Fatalf("auth card text %q", it.Text)
		}
	}
	if got := a.get(); len(got) != 1 || !strings.HasPrefix(got[0], "failed:Failed to authenticate") {
		t.Fatalf("OnAuth calls %v, want one failure", got)
	}
	if err := s.Send("again"); err != nil {
		t.Fatal(err)
	}
	if st := s.Snapshot().State; st != "working" || !strings.Contains(in.String(), "again") {
		t.Fatalf("after Send: state %q, wrote %q", st, in.String())
	}
}

// Without the marked message the words of the result say it, as with the limit.
func TestAuthFailedByWords(t *testing.T) {
	for _, text := range []string{
		"Failed to authenticate: this login reached its time limit and cannot be renewed. Run `claude auth login`",
		"Invalid API key · Please run /login",
	} {
		var a authCalls
		s, _ := limitSession(Options{OnAuth: a.on})
		s.handle([]byte(`{"type":"result","subtype":"success","is_error":true,"result":` + quote(text) + `}`))
		if n := itemKinds(s); n["auth"] != 1 || n["error"] != 0 || s.Snapshot().State != "auth" || len(a.get()) != 1 {
			t.Errorf("%q: items %v, state %q, calls %v", text, n, s.Snapshot().State, a.get())
		}
		s.Stop()
	}
}

func quote(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }

// Another error stays an error: the state is «done», Mako is not told of any login.
func TestOtherErrorStaysError(t *testing.T) {
	var a authCalls
	s, _ := limitSession(Options{OnAuth: a.on})
	defer s.Stop()
	s.handle([]byte(`{"type":"result","subtype":"success","is_error":true,"result":"Prompt is too long","duration_ms":10}`))
	if n := itemKinds(s); n["error"] != 1 || n["auth"] != 0 {
		t.Fatalf("items %v, want one error and no auth", n)
	}
	if st := s.Snapshot().State; st != "done" {
		t.Fatalf("state %q, want done", st)
	}
	if got := a.get(); len(got) != 0 {
		t.Fatalf("OnAuth calls %v, want none", got)
	}
}

// The mark belongs to its turn: a marked message with an error result of something else does not leak into the
// next turn, and a turn that went well tells Mako the login works.
func TestAuthMarkEndsWithTurn(t *testing.T) {
	var a authCalls
	s, _ := limitSession(Options{OnAuth: a.on})
	defer s.Stop()
	s.handle([]byte(authAssistant))
	s.handle([]byte(authResult))
	s.handle([]byte(okAssistant))
	s.handle([]byte(okResult))
	if n := itemKinds(s); n["auth"] != 1 || n["text"] != 1 || n["error"] != 0 {
		t.Fatalf("items %v", n)
	}
	if st := s.Snapshot().State; st != "done" {
		t.Fatalf("state %q, want done", st)
	}
	if got := a.get(); len(got) != 2 || got[1] != "ok" {
		t.Fatalf("OnAuth calls %v, want a failure and then ok", got)
	}
}

// A session in the «auth» state is idle like a «done» one: it may close, and Snapshot names it.
func TestAuthStateIsIdle(t *testing.T) {
	s, _ := limitSession(Options{})
	defer s.Stop()
	s.handle([]byte(authAssistant))
	s.handle([]byte(authResult))
	if st, _, _ := s.State(); st != "auth" {
		t.Fatalf("State %q, want auth", st)
	}
}

// The marked message is not lost when the result does not confirm it: a result that is not an error still ends the
// turn as a failed login, with the message's words.
func TestAuthMarkedWithoutErrorResult(t *testing.T) {
	var a authCalls
	s, _ := limitSession(Options{OnAuth: a.on})
	defer s.Stop()
	s.handle([]byte(authAssistant))
	s.handle([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"","duration_ms":10}`))
	n := itemKinds(s)
	if n["auth"] != 1 || n["text"] != 0 || s.Snapshot().State != "auth" {
		t.Fatalf("items %v, state %q", n, s.Snapshot().State)
	}
	for _, it := range s.Snapshot().Items {
		if it.Kind == "auth" && !strings.Contains(it.Text, "Failed to authenticate") {
			t.Fatalf("card text %q", it.Text)
		}
	}
	if got := a.get(); len(got) != 1 || !strings.HasPrefix(got[0], "failed:") {
		t.Fatalf("OnAuth calls %v", got)
	}
}

// The process dies after the marked message, before any result: the failure is still shown and told to Mako.
func TestAuthMarkedProcessEnds(t *testing.T) {
	var a authCalls
	s, _ := limitSession(Options{OnAuth: a.on})
	defer s.Stop()
	s.handle([]byte(authAssistant))
	s.authLost() // what run does when the process has ended
	n := itemKinds(s)
	if n["auth"] != 1 || n["text"] != 0 {
		t.Fatalf("items %v", n)
	}
	if got := a.get(); len(got) != 1 || !strings.Contains(got[0], "Failed to authenticate") {
		t.Fatalf("OnAuth calls %v", got)
	}
	s.authLost() // once only
	if n := itemKinds(s); n["auth"] != 1 || len(a.get()) != 1 {
		t.Fatalf("told twice: %v %v", n, a.get())
	}
}
