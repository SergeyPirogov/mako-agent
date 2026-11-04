package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
)

const authFailedText = "Failed to authenticate. API Error: 401 Invalid bearer token"

// The Claude login fails: no new task starts, whatever the WIP limit says, and the board hears of it once. The
// first turn of any agent that ends well lifts the hold, and the queue starts the task.
func TestQueueWaitsForLogin(t *testing.T) {
	d, _, jobs := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	told := toldOf(t)
	ctx := context.Background()
	d.Poll(ctx)

	d.NoteAuth(true, authFailedText)
	d.NoteAuth(true, authFailedText) // a second agent fails the same way: nothing new to tell
	if h := d.AuthHold(); h == nil || h.Text != authFailedText || h.At == 0 {
		t.Fatalf("hold %+v", h)
	}
	stopped := func() (out []notify.Notice) {
		for _, n := range told() {
			if n.Kind == notify.Stopped {
				out = append(out, n)
			}
		}
		return out
	}
	waitFor(t, func() bool { return len(stopped()) > 0 }, "no notification")
	if n := stopped(); len(n) != 1 || !strings.Contains(n[0].Message, "claude auth login") {
		t.Fatalf("notifications %+v", n)
	}

	d.Take(ctx, "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.Queued || len(*jobs) != 0 {
		t.Fatalf("while the login fails: %s, %d handed", r.Status, len(*jobs))
	}
	if why := d.QueueReason(); !strings.Contains(why, "claude auth login") {
		t.Fatalf("queued for %q", why)
	}

	d.NoteAuth(false, "") // a turn of some agent ended well
	if d.AuthHold() != nil {
		t.Fatal("the hold is still there after a good turn")
	}
	d.schedule(ctx)
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.HandedOff || len(*jobs) != 1 {
		t.Fatalf("after the login works: %s, %d handed", r.Status, len(*jobs))
	}
	if !strings.Contains(logText(d, "mako"), "login works again") {
		t.Fatalf("log: %v", d.Logs("mako"))
	}
}

// «I logged in — continue» lifts the hold at once; pressed again, it says there was nothing to lift.
func TestClearAuth(t *testing.T) {
	d, _, jobs := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	ctx := context.Background()
	d.Poll(ctx)
	d.NoteAuth(true, authFailedText)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	if len(*jobs) != 0 {
		t.Fatalf("%d handed while the login fails", len(*jobs))
	}
	if !d.ClearAuth() || d.AuthHold() != nil {
		t.Fatal("the hold was not lifted")
	}
	if d.ClearAuth() {
		t.Fatal("lifted a hold that was not there")
	}
	d.schedule(ctx)
	d.Wait()
	if len(*jobs) != 1 {
		t.Fatalf("%d handed after the hold was lifted", len(*jobs))
	}
}

// A good turn when nothing was held tells nothing and holds nothing.
func TestGoodTurnWithoutHold(t *testing.T) {
	d, _, _ := setup(t)
	told := toldOf(t)
	d.NoteAuth(false, "")
	if d.AuthHold() != nil || len(told()) != 0 {
		t.Fatalf("hold %+v, told %+v", d.AuthHold(), told())
	}
}

// A chat whose claude prints what it printed with an invalid token (a marked message, then the error result): the
// session shows one card, the task's agent is stopped with the reason, and the queue is held.
func TestChatLoginFailed(t *testing.T) {
	d, _, _ := setup(t)
	d.Chats = chat.NewManager(filepath.Join(t.TempDir(), "limits.json"))
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nread init\nread msg\n" +
		`echo '{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"` + authFailedText + `"}]},"parent_tool_use_id":null,"error":"authentication_failed"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":true,"result":"` + authFailedText + `","duration_ms":10}'` + "\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := chat.Start(chat.Options{ID: "grp/app#5", Worktree: t.TempDir(), Command: bin, Prompt: "work", OnAuth: d.NoteAuth})
	d.Chats.Put(s)
	t.Cleanup(func() { s.Stop() })
	waitFor(t, func() bool { st, _, _ := s.State(); return st == "auth" }, "the chat is not in the auth state")
	n := map[string]int{}
	for _, it := range s.Snapshot().Items {
		n[it.Kind]++
	}
	if n["auth"] != 1 || n["text"] != 0 || n["error"] != 0 {
		t.Fatalf("items %v", n)
	}
	if d.AuthHold() == nil {
		t.Fatal("the queue is not held")
	}
	a := d.lookChat(core.Record{ID: "grp/app#5"}, Agent{})
	if a.State != "stopped" || !strings.Contains(a.Reason, "claude auth login") {
		t.Fatalf("agent %+v", a)
	}
}
