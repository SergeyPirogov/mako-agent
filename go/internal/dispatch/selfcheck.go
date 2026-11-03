package dispatch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// selfLook: the commits between two fixed SHAs never change, so the count is kept by the pair.
type selfLook struct {
	from, head string
	behind     int
}

// SelfCheck is the board's «Check it yourself» tick: checked saves that you looked at the branch as it is now
// (its HEAD goes in the task), unchecked takes the tick away. The tick is the task's, not the browser's.
func (d *Dispatcher) SelfCheck(id string, checked bool) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !checked {
		d.Store.Update(id, func(r *core.Record) { r.SelfCheck = nil })
		d.Log(id, "self-check cleared")
		return nil
	}
	if !reviewable(r) {
		return errors.New("the check can only be set for Mako agents that are in progress or in an open MR")
	}
	wt, _, err := d.workOf(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); err != nil || head == "" {
		return errors.New("could not read the branch")
	}
	d.Store.Update(id, func(r *core.Record) { r.SelfCheck = &core.SelfCheck{At: now(), Commit: head} })
	d.Log(id, "self-check set at "+head)
	return nil
}

// SelfCheckBehind: how many commits the branch got after your «Check it yourself» tick; 0 when there is no
// tick or it is up to date. A tick whose commit is gone from the branch (a rebase, a force-push) counts as 1.
func (d *Dispatcher) SelfCheckBehind(r core.Record) int {
	sc := r.SelfCheck
	if sc == nil || sc.Commit == "" {
		return 0
	}
	head, _ := d.lookBranch(r)
	if head == "" || head == sc.Commit {
		return 0
	}
	d.mu.Lock()
	l, ok := d.selfBehind[r.ID]
	d.mu.Unlock()
	if ok && l.from == sc.Commit && l.head == head {
		return l.behind
	}
	l = selfLook{from: sc.Commit, head: head, behind: 1}
	if wt, _, err := d.workOf(r.ID); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if n, err := gitx.Run(ctx, wt, "rev-list", "--count", sc.Commit+".."+head); err == nil {
			if c, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && c > 0 {
				l.behind = c
			}
		}
		cancel()
	}
	d.mu.Lock()
	d.selfBehind[r.ID] = l
	d.mu.Unlock()
	return l.behind
}
