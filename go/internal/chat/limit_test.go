package chat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipe: the stdin of a session with no process; what Send writes can be read back.
type pipe struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (p *pipe) Write(b []byte) (int, error) { p.mu.Lock(); defer p.mu.Unlock(); return p.buf.Write(b) }
func (p *pipe) Close() error                { return nil }
func (p *pipe) String() string              { p.mu.Lock(); defer p.mu.Unlock(); return p.buf.String() }

// limitSession: a session in the middle of a turn that is fed claude's lines by hand.
func limitSession(o Options) (*Session, *pipe) {
	in := &pipe{}
	s := &Session{ID: "lim", state: "working", since: time.Now(), pending: map[string]json.RawMessage{}, o: o, mode: "default", stdin: in}
	return s, in
}

func rejected(resetsAt int64) string {
	b, _ := json.Marshal(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "resetsAt": resetsAt, "rateLimitType": "five_hour"}})
	return string(b)
}

const limitResult = `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 3pm","duration_ms":10}`

// The limit cuts the turn: a rejected rate-limit event and the error result after it make the state «limited» with
// the time it resets; a message of yours clears it.
func TestLimitedTurn(t *testing.T) {
	s, in := limitSession(Options{})
	defer s.Stop()
	reset := time.Now().Add(2 * time.Hour).Unix()
	s.handle([]byte(rejected(reset)))
	s.handle([]byte(limitResult))
	snap := s.Snapshot()
	if snap.State != "limited" || snap.LimitedUntil != reset {
		t.Fatalf("state %q until %d, want limited until %d", snap.State, snap.LimitedUntil, reset)
	}
	if err := s.Send("continue"); err != nil {
		t.Fatal(err)
	}
	snap = s.Snapshot()
	if snap.State != "working" || snap.LimitedUntil != 0 || !strings.Contains(in.String(), "continue") {
		t.Fatalf("after Send: state %q until %d, wrote %q", snap.State, snap.LimitedUntil, in.String())
	}
}

// Without the event the words of the error say it; the reset is then the fullest window's the session was told of.
func TestLimitedByWords(t *testing.T) {
	var got Limits
	s, _ := limitSession(Options{OnLimits: func(l Limits) { got = l }})
	defer s.Stop()
	soon, later := time.Now().Add(time.Hour).Unix(), time.Now().Add(48*time.Hour).Unix()
	s.handle([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","unifiedWindows":{"five_hour":{"utilization":0.95,"resetsAt":` + itoa(soon) + `},"seven_day":{"utilization":0.4,"resetsAt":` + itoa(later) + `}}}}`))
	if got.FiveHour == nil {
		t.Fatal("the windows are still reported")
	}
	s.handle([]byte(limitResult))
	if snap := s.Snapshot(); snap.State != "limited" || snap.LimitedUntil != soon {
		t.Fatalf("state %q until %d, want limited until %d", snap.State, snap.LimitedUntil, soon)
	}
}

// An error that is not the limit, and a rejected event on a turn that went on to succeed, are not «limited».
func TestNotLimited(t *testing.T) {
	s, _ := limitSession(Options{})
	defer s.Stop()
	s.handle([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 500","duration_ms":10}`))
	if snap := s.Snapshot(); snap.State != "done" || snap.LimitedUntil != 0 {
		t.Fatalf("an ordinary error: state %q until %d", snap.State, snap.LimitedUntil)
	}
	s.setState("working")
	s.handle([]byte(rejected(time.Now().Add(time.Hour).Unix())))
	s.handle([]byte(`{"type":"result","subtype":"success","duration_ms":10}`))
	if snap := s.Snapshot(); snap.State != "done" || snap.LimitedUntil != 0 {
		t.Fatalf("a turn that succeeded: state %q until %d", snap.State, snap.LimitedUntil)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// shortGrace: the continuation comes a moment after the reset, not 30 seconds.
func shortGrace(t *testing.T) {
	old := ResumeGrace
	ResumeGrace = 50 * time.Millisecond
	t.Cleanup(func() { ResumeGrace = old })
}

func limited(t *testing.T, s *Session, reset int64) {
	t.Helper()
	s.handle([]byte(rejected(reset)))
	s.handle([]byte(limitResult))
	if st, _, _ := s.State(); st != "limited" {
		t.Fatalf("state %q", st)
	}
}

// «Continue after the reset»: the session says «go on» by itself once the limit has reset.
func TestResumeOnReset(t *testing.T) {
	shortGrace(t)
	said := make(chan string, 4)
	s, _ := limitSession(Options{OnResume: func(text string) error { said <- text; return nil }})
	defer s.Stop()
	s.SetResumeOnReset(true)
	if !s.Snapshot().ResumeOnReset {
		t.Fatal("the choice is not in the snapshot")
	}
	limited(t, s, time.Now().Unix())
	select {
	case text := <-said:
		if text != ResumeMessage {
			t.Fatalf("said %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not go on after the reset")
	}
}

// Without the choice nothing is sent, and a limit that resets later is waited for.
func TestNoResumeWithoutChoiceOrBeforeReset(t *testing.T) {
	shortGrace(t)
	said := make(chan string, 4)
	s, _ := limitSession(Options{OnResume: func(text string) error { said <- text; return nil }})
	defer s.Stop()
	limited(t, s, time.Now().Unix()) // reset already, but nobody asked
	s.SetResumeOnReset(true)         // asked now: it goes on at once
	select {
	case <-said:
	case <-time.After(5 * time.Second):
		t.Fatal("asked after the limit, it did not go on")
	}
	s2, _ := limitSession(Options{OnResume: func(text string) error { said <- text; return nil }})
	defer s2.Stop()
	s2.SetResumeOnReset(true)
	limited(t, s2, time.Now().Add(time.Hour).Unix())
	select {
	case <-said:
		t.Fatal("went on before the limit reset")
	case <-time.After(300 * time.Millisecond):
	}
}

// Stop, a message of yours and taking the choice back all cancel the continuation.
func TestResumeCancelled(t *testing.T) {
	shortGrace(t)
	for name, cancel := range map[string]func(*Session){
		"stop":    func(s *Session) { s.Stop() },
		"message": func(s *Session) { s.Send("my own") },
		"off":     func(s *Session) { s.SetResumeOnReset(false) },
	} {
		said := make(chan string, 4)
		s, _ := limitSession(Options{OnResume: func(text string) error { said <- text; return nil }})
		s.SetResumeOnReset(true)
		limited(t, s, time.Now().Add(150*time.Millisecond).Unix()-1) // due within the grace
		cancel(s)
		select {
		case <-said:
			t.Fatalf("%s: went on all the same", name)
		case <-time.After(600 * time.Millisecond):
		}
		s.Stop()
	}
}

// A session closed for idleness keeps its continuation: it resumes the conversation, which Stop alone would not.
func TestResumeSurvivesSleep(t *testing.T) {
	shortGrace(t)
	said := make(chan string, 4)
	s, _ := limitSession(Options{OnResume: func(text string) error { said <- text; return nil }})
	s.SetResumeOnReset(true)
	limited(t, s, time.Now().Add(200*time.Millisecond).Unix())
	s.sleep()
	if !s.Snapshot().ResumeOnReset || s.asleep != true {
		t.Fatal("it did not sleep")
	}
	select {
	case <-said:
	case <-time.After(5 * time.Second):
		t.Fatal("the sleeping session did not go on")
	}
}

// Without OnResume the session sends the message itself.
func TestResumeSendsItself(t *testing.T) {
	shortGrace(t)
	s, in := limitSession(Options{})
	defer s.Stop()
	s.SetResumeOnReset(true)
	limited(t, s, time.Now().Unix())
	eventually(t, func() bool { return strings.Contains(in.String(), ResumeMessage) }, func() string { return "nothing written: " + in.String() })
	if st, _, _ := s.State(); st != "working" {
		t.Fatalf("state %q", st)
	}
}

// A transcript replayed with such a break shows when the limit resets; the state stays «exited» (nobody runs it).
func TestReplayLimited(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(path, []byte(`{"type":"user","message":{"content":"Go"}}
`+rejected(reset)+"\n"+limitResult+"\n"), 0o600)
	snap := ReplayIn("x", "", path)
	if snap.State != "exited" || snap.LimitedUntil != reset {
		t.Fatalf("state %q until %d, want until %d", snap.State, snap.LimitedUntil, reset)
	}
}
