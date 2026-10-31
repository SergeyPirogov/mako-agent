package dispatch

import (
	"context"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Blocked: why a task in work or in review cannot go on by itself, in words, or "" when nothing holds it.
// The board shows such tasks under «Заблоковані»: an agent waiting for your answer or permission, an MR whose
// CI failed or that conflicts with its target. It reads only what Mako already looked at, never the network.
func (d *Dispatcher) Blocked(r core.Record) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Column(0) {
	case "doing":
		if a := d.agents[r.ID]; a != nil && a.State == "waiting" {
			return "агент чекає на вас" + orBlank(": ", a.Reason)
		}
	case "review":
		l, ok := d.reviews[r.ID]
		if !ok || l.err != nil || l.check == nil || l.check.State != "open" {
			return ""
		}
		switch {
		case l.check.Conflicts:
			return "конфлікти в MR"
		case l.check.CI == "failed":
			return "CI не пройшов"
		}
	}
	return ""
}

// CheckBlocked looks at the MRs in review, so a failed CI or a conflict shows on the board without opening
// the MR card; Review keeps the host from being asked more often than reviewFresh.
func (d *Dispatcher) CheckBlocked(ctx context.Context) {
	for _, r := range d.Store.All() {
		if r.ArchivedAt != 0 || r.Column(0) != "review" {
			continue
		}
		d.Review(ctx, r.ID)
	}
}
