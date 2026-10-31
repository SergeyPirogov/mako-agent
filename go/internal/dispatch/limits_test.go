package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestLimitHold(t *testing.T) {
	now := time.Unix(1000, 0)
	l := chat.Limits{FiveHour: &chat.Window{Used: 0.9, ResetsAt: 2000}, Week: &chat.Window{Used: 0.5, ResetsAt: 9000}}
	if h := limitHold(l, 85, now); h == nil || h.Window != "5 год" || h.Used != 90 || h.ResetsAt != 2000 {
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
	if h := limitHold(l, 85, now); h == nil || h.Window != "тиждень" || h.ResetsAt != 9000 {
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
