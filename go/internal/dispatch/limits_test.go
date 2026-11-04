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

// logText: the task's log, as one text.
func logText(d *Dispatcher, id string) string {
	var b strings.Builder
	for _, l := range d.Logs(id) {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

// limitedTask: a task whose agent the Claude limit cut and that is kept so (no session: as after Mako restarted).
func limitedTask(d *Dispatcher, id, priority string, until int64, resume bool) {
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Task.Priority, r.LimitedUntil, r.ResumeAfterLimit = core.HandedOff, priority, until, resume
	})
}

// toldOf: the notifications Mako sends from now on.
func toldOf(t *testing.T) func() []notify.Notice {
	var mu sync.Mutex
	var told []notify.Notice
	notify.Remote = func(n notify.Notice) { mu.Lock(); told = append(told, n); mu.Unlock() }
	t.Cleanup(func() { notify.Remote = nil })
	return func() []notify.Notice { mu.Lock(); defer mu.Unlock(); return append([]notify.Notice{}, told...) }
}

// After a restart the agents the limit cut go on by themselves once it has reset: the most urgent first, no more
// at once than the WIP limit (2), one notification for them all and a line in each task's log; the rest waits
// until a slot frees.
func TestResumeLimitedAfterRestart(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	told := toldOf(t)
	var mu sync.Mutex
	var went []string
	d.goOn = func(id string) error { mu.Lock(); went = append(went, id); mu.Unlock(); return nil }
	past := time.Now().Add(-time.Minute).Unix()
	limitedTask(d, "grp/app#1", "Low", past, true)
	limitedTask(d, "grp/app#2", "High", past, true)
	limitedTask(d, "grp/app#3", "Blocker", past, true)
	if n := d.resumeLimited(time.Now()); n != 2 || strings.Join(went, " ") != "grp/app#3 grp/app#2" {
		t.Fatalf("went on %d: %v", n, went)
	}
	for _, id := range []string{"grp/app#3", "grp/app#2"} {
		if r, _ := d.Store.Get(id); r.LimitedUntil != 0 {
			t.Fatalf("%s is still kept as cut", id)
		}
		if !strings.Contains(logText(d, id), "the agent goes on by itself") {
			t.Fatalf("%s log: %v", id, d.Logs(id))
		}
	}
	waitFor(t, func() bool { return len(told()) > 0 }, "no notification")
	if n := told(); len(n) != 1 || n[0].Kind != notify.Limits || !strings.Contains(n[0].Message, "2") {
		t.Fatalf("notifications %+v", n)
	}
	// Both slots are taken: the third waits.
	if n := d.resumeLimited(time.Now()); n != 0 {
		t.Fatalf("went on beyond the WIP limit: %v", went)
	}
	if r, _ := d.Store.Get("grp/app#1"); r.LimitedUntil != past {
		t.Fatal("the waiting one was forgotten")
	}
	d.Store.Update("grp/app#3", func(r *core.Record) { r.Status = core.Done })
	if n := d.resumeLimited(time.Now()); n != 1 || went[2] != "grp/app#1" {
		t.Fatalf("after a slot freed: %v", went)
	}
}

// Nothing goes on while a limit is still past the threshold (the week's), before the reset, or when the chat
// turned going on by itself off; a task that finished meanwhile is forgotten.
func TestResumeLimitedWaits(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	toldOf(t)
	var went []string
	d.goOn = func(id string) error { went = append(went, id); return nil }
	limitedTask(d, "grp/app#1", "", time.Now().Add(time.Hour).Unix(), true)   // not yet
	limitedTask(d, "grp/app#2", "", time.Now().Add(-time.Hour).Unix(), false) // off for this chat
	limitedTask(d, "grp/app#3", "", time.Now().Add(-time.Hour).Unix(), true)
	d.Store.Update("grp/app#3", func(r *core.Record) { r.Status = core.Cancelled })
	d.Chats.SetLimits(chat.Limits{Week: &chat.Window{Used: 0.97, ResetsAt: time.Now().Add(48 * time.Hour).Unix()}})
	if n := d.resumeLimited(time.Now()); n != 0 {
		t.Fatalf("went on past the week's threshold: %v", went)
	}
	d.Chats.SetLimits(chat.Limits{Week: &chat.Window{Used: 0.5, ResetsAt: time.Now().Add(48 * time.Hour).Unix()}})
	if n := d.resumeLimited(time.Now()); n != 0 {
		t.Fatalf("went on: %v", went)
	}
	if r, _ := d.Store.Get("grp/app#3"); r.LimitedUntil != 0 {
		t.Fatal("a cancelled task is still kept as cut")
	}
	if r, _ := d.Store.Get("grp/app#2"); r.LimitedUntil == 0 {
		t.Fatal("the one turned off was forgotten")
	}
}

// A message that could not be given is not claimed as going on: no notification, the task's log says why, and it
// is not tried again every round.
func TestResumeLimitedFails(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	told := toldOf(t)
	d.goOn = func(string) error { return errors.New("no worktree") }
	limitedTask(d, "grp/app#1", "", time.Now().Add(-time.Hour).Unix(), true)
	if n := d.resumeLimited(time.Now()); n != 0 {
		t.Fatalf("went on %d", n)
	}
	time.Sleep(100 * time.Millisecond) // a notification goes out in the background
	if len(told()) != 0 {
		t.Fatalf("told %v", told())
	}
	if !strings.Contains(logText(d, "grp/app#1"), "could not go on after the Claude limit: no worktree") {
		t.Fatalf("log %v", d.Logs("grp/app#1"))
	}
	if r, _ := d.Store.Get("grp/app#1"); r.LimitedUntil != 0 {
		t.Fatal("it would be tried again every round")
	}
}

// The agent's limit is kept with the task as its chat tells it, with the settings' choice (on by default); taking
// it back on one chat leaves the others; Stop forgets it; a chat without a running agent keeps the choice too.
func TestLimitKept(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	reset := time.Now().Add(time.Hour).Unix()
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Status = core.HandedOff })
	if !d.resumeFlag("grp/app#5") {
		t.Fatal("going on after the limit is not on by default")
	}
	limitedChat(t, d, "grp/app#5", reset, chat.Options{
		OnLimit:       func(u int64, r bool) { d.noteLimit("grp/app#5", u, r) },
		ResumeOnReset: d.resumeFlag("grp/app#5"),
	})
	waitFor(t, func() bool { r, _ := d.Store.Get("grp/app#5"); return r.LimitedUntil == reset && r.ResumeAfterLimit }, "the limit is not kept")
	if !strings.Contains(logText(d, "grp/app#5"), "it goes on by itself after "+time.Unix(reset, 0).Format("15:04")) {
		t.Fatalf("log %v", d.Logs("grp/app#5"))
	}
	limitedTask(d, "grp/app#6", "", reset, true)
	if err := d.SetResumeOnReset("grp/app#5", false); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get("grp/app#5"); r.ResumeAfterLimit || r.LimitedUntil != reset {
		t.Fatalf("taken back: %+v", r)
	}
	if r, _ := d.Store.Get("grp/app#6"); !r.ResumeAfterLimit {
		t.Fatal("the other chat lost its choice")
	}
	// No running agent: the kept choice changes.
	if err := d.SetResumeOnReset("grp/app#6", false); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get("grp/app#6"); r.ResumeAfterLimit {
		t.Fatal("the choice of a chat with no agent is not kept")
	}
	if snap := d.WithLimit("grp/app#6", chat.Snapshot{State: "exited", Version: 7}); snap.State != "limited" || snap.LimitedUntil != reset || snap.ResumeOnReset || snap.Version == 7 {
		t.Fatalf("snapshot %+v", snap)
	}
	d.StopTerminal("grp/app#5")
	if r, _ := d.Store.Get("grp/app#5"); r.LimitedUntil != 0 {
		t.Fatal("stopped, it would still go on by itself")
	}
	if err := d.SetResumeOnReset("chat-none", true); err == nil {
		t.Fatal("no error for a chat that does not exist")
	}
}

// A free chat's limit is kept in chats.json and it goes on the free chat's way, after the tasks.
func TestResumeLimitedChats(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	toldOf(t)
	d.updateChats(func([]FreeChat) []FreeChat { return []FreeChat{{ID: "chat-a", Title: "Fix login"}} })
	past := time.Now().Add(-time.Hour).Unix()
	d.noteLimit("chat-a", past, true)
	if c, _ := d.freeChat("chat-a"); c.LimitedUntil != past || !c.ResumeAfterLimit {
		t.Fatalf("chat %+v", c)
	}
	limitedTask(d, "grp/app#1", "Low", past, true)
	var went []string
	d.goOn = func(id string) error { went = append(went, id); return nil }
	if n := d.resumeLimited(time.Now()); n != 2 || strings.Join(went, " ") != "grp/app#1 chat-a" {
		t.Fatalf("went on %v", went)
	}
	if c, _ := d.freeChat("chat-a"); c.LimitedUntil != 0 {
		t.Fatal("the chat is still kept as cut")
	}
}

func TestPriorityRank(t *testing.T) {
	for p, want := range map[string]int{"P0": 0, "Blocker": 0, "Highest": 0, "high": 1, "P1": 1, "": 2, "Medium": 2, "whatever": 2, "Low": 3, "P4": 4, "Lowest": 4} {
		if got := priorityRank(p); got != want {
			t.Fatalf("%q: %d, want %d", p, got, want)
		}
	}
}
