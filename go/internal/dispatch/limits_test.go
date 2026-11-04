package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func TestLimitHold(t *testing.T) {
	now := time.Unix(1000, 0)
	l := chat.Limits{FiveHour: &chat.Window{Used: 0.9, ResetsAt: 2000}, Week: &chat.Window{Used: 0.5, ResetsAt: 9000}}
	if h := limitHold(l, 85, now); h == nil || h.Window != "5 h" || h.Used != 90 || h.ResetsAt != 2000 {
		t.Fatalf("5 hours past the threshold: %+v", h)
	}
	if h := limitHold(l, 95, now); h != nil {
		t.Fatalf("below the threshold: %+v", h)
	}
	if h := limitHold(l, 0, now); h != nil {
		t.Fatalf("threshold off: %+v", h)
	}
	if h := limitHold(l, 85, time.Unix(2000, 0)); h != nil {
		t.Fatalf("the window has reset: %+v", h)
	}
	l.Week.Used = 0.97
	if h := limitHold(l, 85, now); h == nil || h.Window != "week" || h.ResetsAt != 9000 {
		t.Fatalf("both full, the later reset: %+v", h)
	}
}

// Past the threshold a taken task waits in the queue; once the limit resets it starts by itself.
func TestQueueWaitsForLimit(t *testing.T) {
	d, _, jobs := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	ctx := context.Background()
	d.Poll(ctx)
	reset := time.Now().Add(2 * time.Second).Unix()
	d.Chats.SetLimits(chat.Limits{FiveHour: &chat.Window{Used: 0.9, ResetsAt: reset}})
	d.Take(ctx, "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.Queued || len(*jobs) != 0 {
		t.Fatalf("near the limit: %s, %d handed", r.Status, len(*jobs))
	}
	if d.LimitHold() == nil {
		t.Fatal("the board shows no hold")
	}
	if why := d.QueueReason(); !strings.HasPrefix(why, "Claude limit (5 h) used up to 90%") || d.Starting("grp/app#5") != nil {
		t.Fatalf("queued for %q", why)
	}
	for time.Now().Unix() <= reset {
		time.Sleep(100 * time.Millisecond)
	}
	d.schedule(ctx)
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.HandedOff || len(*jobs) != 1 {
		t.Fatalf("after the reset: %s, %d handed", r.Status, len(*jobs))
	}
}

// «Запустити зараз» starts a queued task while the limit still holds the queue.
func TestPushPastLimit(t *testing.T) {
	d, _, jobs := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	ctx := context.Background()
	d.Poll(ctx)
	d.Chats.SetLimits(chat.Limits{FiveHour: &chat.Window{Used: 0.9, ResetsAt: time.Now().Add(time.Hour).Unix()}})
	d.Take(ctx, "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.Queued {
		t.Fatalf("near the limit: %s", r.Status)
	}
	d.Push(ctx, "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.HandedOff || len(*jobs) != 1 {
		t.Fatalf("pushed: %s, %d handed", r.Status, len(*jobs))
	}
}

// A chat's agent that the Claude limit cut: the stand-in claude prints a rejected rate-limit event and the error
// result that follows it, then waits. reset: when the limit resets (unix).
func limitedChat(t *testing.T, d *Dispatcher, id string, reset int64, opts chat.Options) *chat.Session {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nread init\nread msg\n" +
		`echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":` + strconv.FormatInt(reset, 10) + `}}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":true,"result":"You have hit your session limit","duration_ms":10}'` + "\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	opts.ID, opts.Worktree, opts.Command, opts.Prompt = id, t.TempDir(), bin, "work"
	s := chat.Start(opts)
	d.Chats.Put(s)
	t.Cleanup(func() { s.Stop() })
	waitFor(t, func() bool { st, _, _ := s.State(); return st == "limited" }, "the chat is not limited")
	return s
}

// The task's agent whose turn the limit cut is stopped (not finished: no checks, no review on half the work), and
// the reason says when the limit resets.
func TestLookChatLimited(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	reset := time.Now().Add(time.Hour).Unix()
	limitedChat(t, d, "grp/app#5", reset, chat.Options{})
	a := d.lookChat(core.Record{ID: "grp/app#5"}, Agent{})
	if a.State != "stopped" || !a.Live || a.Reason != "the Claude limit until "+time.Unix(reset, 0).Format("15:04") {
		t.Fatalf("agent %+v", a)
	}
}

// «Продовжити після скидання»: the limit resets, the agent is said «go on» the way a message from the board goes,
// and Mako tells you; a failed message tells nothing.
func TestResumeAfterLimitNotifies(t *testing.T) {
	old := chat.ResumeGrace
	chat.ResumeGrace = 20 * time.Millisecond
	defer func() { chat.ResumeGrace = old }()
	var mu sync.Mutex
	var told []notify.Notice
	notify.Remote = func(n notify.Notice) { mu.Lock(); told = append(told, n); mu.Unlock() }
	defer func() { notify.Remote = nil }()
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	said := make(chan string, 4)
	fail := errors.New("no worktree")
	var err error
	say := func(text string) error { said <- text; return err }
	limitedChat(t, d, "chat-a", time.Now().Unix(), chat.Options{OnResume: d.afterLimit("Fix login", say)})
	if e := d.SetResumeOnReset("chat-a", true); e != nil {
		t.Fatal(e)
	}
	select {
	case text := <-said:
		if text != chat.ResumeMessage {
			t.Fatalf("said %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not go on")
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(told) == 1 }, "no notification")
	if n := told[0]; n.Kind != notify.Limits || !strings.Contains(n.Message, "Fix login") {
		t.Fatalf("notification %+v", n)
	}
	// A message that could not be given: Mako does not claim the agent went on.
	err = fail
	if e := d.afterLimit("x", say)("go on"); !errors.Is(e, fail) {
		t.Fatalf("error %v", e)
	}
	mu.Lock()
	n := len(told)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("told %d times", n)
	}
}

// Nothing runs to wait for the reset: the choice is refused, not silently dropped.
func TestResumeOnResetNoSession(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	if err := d.SetResumeOnReset("chat-none", true); err == nil {
		t.Fatal("no error for a chat with no session")
	}
}

// An agent the Claude limit cut is not "done" for what Mako sends by itself: a fix for the MR would only run into
// the limit again, and clear the state that tells you of it.
func TestAutoFixSkipsLimitedAgent(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	cfg := *d.Config()
	cfg.Terminal.AutoFix = true
	d.Cfg = &cfg
	wt := t.TempDir()
	id := "grp/app#5"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Executor, r.Worktree, r.View = core.Done, "terminal", wt, "chat"
		r.Task = core.Task{ID: id, TrackerID: "gitlab", Project: "grp/app", Title: "Кнопка"}
		r.PRURL, r.Branch = "https://gitlab.example/grp/app/-/merge_requests/1", "fix/5-knopka"
	})
	s := limitedChat(t, d, id, time.Now().Add(time.Hour).Unix(), chat.Options{})
	if a := d.lookTerminal(core.Record{ID: id, View: "chat"}, time.Now()); !a.Limited || a.turnOver() {
		t.Fatalf("agent %+v", a)
	}
	d.mu.Lock()
	d.reviews[id] = reviewLook{at: time.Now(), check: &trackers.ReviewCheck{State: "open", Conflicts: true}}
	d.mu.Unlock()
	d.AutoFix(context.Background(), time.Now())
	for _, it := range s.Snapshot().Items {
		if it.Kind == "user" && strings.Contains(it.Text, "конфліктує") {
			t.Fatalf("the limited agent was asked to fix: %q", it.Text)
		}
	}
	if st, _, _ := s.State(); st != "limited" {
		t.Fatalf("state %q", st)
	}
	// An agent that finished, or was closed, is told.
	for _, state := range []string{"done", "stopped"} {
		if !(Agent{State: state}).turnOver() {
			t.Fatalf("%s: not over", state)
		}
	}
	for _, state := range []string{"working", "waiting"} {
		if (Agent{State: state}).turnOver() {
			t.Fatalf("%s: over", state)
		}
	}
}
