package dispatch

import (
	"context"
	"sort"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Mako's own agents (chat and terminal) are its child processes: a restart ends them. «Перезапустити, коли
// агенти закінчать хід» waits until none of them is in the middle of a step, so no step is lost; an agent whose
// turn ended continues after the restart from its conversation (claude --resume). Agents in Orca do not care.

// BusyTasks: the tasks whose agent in Mako works now, or that are being handed off.
func (d *Dispatcher) BusyTasks() []string {
	busy := map[string]bool{}
	d.mu.Lock()
	for id := range d.starting {
		busy[id] = true
	}
	d.mu.Unlock()
	for _, r := range d.Store.All() {
		if r.Status != core.HandedOff || !onTerminal(r) {
			continue
		}
		if c := d.Chats.Get(r.ID); c != nil {
			if st, _, _ := c.State(); st == "working" {
				busy[r.ID] = true
			}
		}
		if s := d.Sessions.Get(r.ID); s != nil && s.State().State == "working" {
			busy[r.ID] = true
		}
	}
	ids := make([]string, 0, len(busy))
	for id := range busy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RequestRestart asks for a restart once no agent works (on), or takes the request back.
func (d *Dispatcher) RequestRestart(on bool) {
	d.mu.Lock()
	d.restartPending = on
	d.mu.Unlock()
	if on {
		d.Log("mako", "restart once the agents finish their turn")
	}
}

func (d *Dispatcher) RestartPending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.restartPending
}

// WaitRestart returns true once a restart is asked for and no agent works; false when ctx ends first.
func (d *Dispatcher) WaitRestart(ctx context.Context, every time.Duration) bool {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if d.RestartPending() && len(d.BusyTasks()) == 0 {
			d.Log("mako", "the agents finished their turn: restarting")
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
