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

func limited(t *testing.T, s *Session, reset int64) {
	t.Helper()
	s.handle([]byte(rejected(reset)))
	s.handle([]byte(limitResult))
	if st, _, _ := s.State(); st != "limited" {
		t.Fatalf("state %q", st)
	}
}

type limitNote struct {
	until  int64
	resume bool
}

// noted: an OnLimit that keeps what it was told, for Mako to keep.
func noted() (func(int64, bool), func() []limitNote) {
	var mu sync.Mutex
	var got []limitNote
	return func(u int64, r bool) { mu.Lock(); got = append(got, limitNote{u, r}); mu.Unlock() },
		func() []limitNote { mu.Lock(); defer mu.Unlock(); return append([]limitNote{}, got...) }
}

// The session tells Mako when the limit cuts it, with the choice it started with (the settings'), and when a
// message goes on from it; the session itself sends nothing after the reset.
func TestOnLimitTold(t *testing.T) {
	on, got := noted()
	s, in := limitSession(Options{OnLimit: on, ResumeOnReset: true})
	s.resumeOnReset = s.o.ResumeOnReset // limitSession builds it without Start
	defer s.Stop()
	reset := time.Now().Unix()
	limited(t, s, reset)
	if g := got(); len(g) != 1 || g[0] != (limitNote{reset, true}) {
		t.Fatalf("told %v", g)
	}
	if !s.Snapshot().ResumeOnReset {
		t.Fatal("the choice is not in the snapshot")
	}
	time.Sleep(100 * time.Millisecond)
	if in.String() != "" {
		t.Fatalf("the session went on by itself: %q", in.String())
	}
	if err := s.Send("my own"); err != nil {
		t.Fatal(err)
	}
	if g := got(); len(g) != 2 || g[1].until != 0 {
		t.Fatalf("after Send: told %v", g)
	}
}

// Taking the choice back on one chat is told for it alone; another chat keeps its own.
func TestResumeOnResetPerChat(t *testing.T) {
	onA, gotA := noted()
	onB, gotB := noted()
	a, _ := limitSession(Options{OnLimit: onA})
	b, _ := limitSession(Options{OnLimit: onB})
	a.resumeOnReset, b.resumeOnReset = true, true
	defer a.Stop()
	defer b.Stop()
	reset := time.Now().Add(time.Hour).Unix()
	limited(t, a, reset)
	limited(t, b, reset)
	a.SetResumeOnReset(false)
	if a.Snapshot().ResumeOnReset || !b.Snapshot().ResumeOnReset {
		t.Fatalf("a %v, b %v", a.Snapshot().ResumeOnReset, b.Snapshot().ResumeOnReset)
	}
	if g := gotA(); g[len(g)-1] != (limitNote{reset, false}) {
		t.Fatalf("a told %v", g)
	}
	if g := gotB(); len(g) != 1 || !g[0].resume {
		t.Fatalf("b told %v", g)
	}
}

// Stop (Mako quitting) does not tell the limit is gone: it is kept, so the agent goes on after a restart.
func TestStopKeepsLimit(t *testing.T) {
	on, got := noted()
	s, _ := limitSession(Options{OnLimit: on})
	limited(t, s, time.Now().Add(time.Hour).Unix())
	s.Stop()
	if g := got(); len(g) != 1 || g[0].until == 0 {
		t.Fatalf("told %v", g)
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
