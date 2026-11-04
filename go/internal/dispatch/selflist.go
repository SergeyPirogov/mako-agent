package dispatch

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// The «Check it yourself» steps in the MR's description, as a task list between two marks: ticked on the board,
// they are ticked in the MR, and a tick given in the MR comes to the board. Which side changed is told by the
// ticks as Mako last saw them on both (SelfCheck.Synced); when both did, the MR's tick of a step wins.

const (
	selfListStart = "<!-- mako:self-check -->"
	selfListEnd   = "<!-- /mako:self-check -->"
)

// selfSyncEvery: how often an MR's task list is read again; a tick on the board writes it at once.
const selfSyncEvery = time.Minute

// selfListItem: a line of the task list, "- [ ] …" or "- [x] …".
var selfListItem = regexp.MustCompile(`(?m)^\s*[-*]\s+\[([ xX])\]\s`)

// renderSelfList: the block of the steps with their ticks.
func renderSelfList(steps []core.CheckStep, done []bool) string {
	var b strings.Builder
	b.WriteString(selfListStart + "\n" + i18n.TA("mr.selfCheck") + "\n\n")
	for i, s := range steps {
		mark := " "
		if i < len(done) && done[i] {
			mark = "x"
		}
		text := s.Do
		if s.Open != "" {
			text = "`" + strings.ReplaceAll(s.Open, "`", "'") + "`: " + text
		}
		if s.Expect != "" {
			text += " → " + s.Expect
		}
		b.WriteString("- [" + mark + "] " + strings.Join(strings.Fields(text), " ") + "\n")
	}
	b.WriteString(selfListEnd)
	return b.String()
}

// parseSelfList: the ticks of the block in an MR's description, in the order of its lines; false when there is
// no block.
func parseSelfList(description string) ([]bool, bool) {
	block, ok := selfListBlock(description)
	if !ok {
		return nil, false
	}
	var ticks []bool
	for _, m := range selfListItem.FindAllStringSubmatch(block, -1) {
		ticks = append(ticks, m[1] != " ")
	}
	return ticks, true
}

// selfListBlock: the text between the marks.
func selfListBlock(description string) (string, bool) {
	i := strings.Index(description, selfListStart)
	if i < 0 {
		return "", false
	}
	rest := description[i+len(selfListStart):]
	j := strings.Index(rest, selfListEnd)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// withSelfList: the description with block in place of its old one, or after the rest of it.
func withSelfList(description, block string) string {
	i := strings.Index(description, selfListStart)
	if i >= 0 {
		if j := strings.Index(description[i:], selfListEnd); j >= 0 {
			return description[:i] + block + description[i+j+len(selfListEnd):]
		}
	}
	if description = strings.TrimRight(description, "\n "); description == "" {
		return block
	}
	return description + "\n\n" + block
}

// selfSyncable: the task's MR has a task list to keep: an open MR of Mako's agent with steps to check.
func (d *Dispatcher) selfSyncable(r core.Record) bool {
	return reviewable(r) && r.Status == core.Done && r.PRURL != "" && len(d.checkSteps(r)) > 0
}

// SyncSelfLists keeps the task lists of the open MRs and the board's ticks the same, each MR once in a while.
func (d *Dispatcher) SyncSelfLists(ctx context.Context) {
	for _, r := range d.Store.All() {
		if !d.selfSyncable(r) {
			continue
		}
		d.mu.Lock()
		at := d.selfSynced[r.ID]
		d.mu.Unlock()
		if time.Since(at) < selfSyncEvery {
			continue
		}
		d.syncSelf(ctx, r.ID)
	}
}

// syncSelf syncs the task's MR task list now, through its code host; one at a time per task.
func (d *Dispatcher) syncSelf(ctx context.Context, id string) {
	d.mu.Lock()
	if d.selfSyncing[id] {
		d.mu.Unlock()
		return
	}
	d.selfSyncing[id] = true
	d.selfSynced[id] = time.Now()
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.selfSyncing, id)
		d.mu.Unlock()
	}()
	r, ok := d.Store.Get(id)
	if !ok || !d.selfSyncable(r) {
		return
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return
	}
	host, _, err := d.host(ctx, t, repo)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := d.syncSelfList(ctx, host, id); err != nil {
		d.Fault(id, "MR task list not synced", err)
	}
}

// syncSelfNow: a tick on the board goes to the MR at once, in the background.
func (d *Dispatcher) syncSelfNow(id string) {
	go d.syncSelf(context.Background(), id)
}

// syncSelfList brings the MR's task list and the board's ticks of the task together.
func (d *Dispatcher) syncSelfList(ctx context.Context, host trackers.CodeHost, id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return nil
	}
	steps := d.checkSteps(r)
	if len(steps) == 0 || r.PRURL == "" {
		return nil
	}
	board := d.boardTicks(r)
	var synced []bool
	if r.SelfCheck != nil {
		synced = r.SelfCheck.Synced
	}
	title, description, err := host.ReadReview(ctx, r.PRURL)
	if err != nil {
		return err
	}
	mr, found := parseSelfList(description)
	if found && len(mr) == len(steps) && len(synced) == len(steps) && !slices.Equal(mr, synced) {
		// Ticks given or taken away in the MR: they come to the board.
		head, err := d.headOf(id)
		if err != nil {
			return err
		}
		for i := range steps {
			if mr[i] != synced[i] && mr[i] != board[i] {
				d.setTick(id, steps, i, mr[i], head)
				if mr[i] {
					d.Log(id, fmt.Sprintf("self-check step %d ticked in the MR", i+1))
				} else {
					d.Log(id, fmt.Sprintf("self-check step %d cleared in the MR", i+1))
				}
			}
		}
		r, _ = d.Store.Get(id)
		board = d.boardTicks(r)
	}
	if !found || !slices.Equal(mr, board) {
		if err := host.DescribeReview(ctx, r.PRURL, title, withSelfList(description, renderSelfList(steps, board))); err != nil {
			return err
		}
	}
	if !slices.Equal(synced, board) {
		d.Store.UpdateIfExists(id, func(r *core.Record) {
			if r.SelfCheck == nil {
				r.SelfCheck = &core.SelfCheck{}
			}
			r.SelfCheck.Synced = board
		})
	}
	return nil
}

// boardTicks: each step ticked at the branch as it is now (a stale tick is no tick in the MR).
func (d *Dispatcher) boardTicks(r core.Record) []bool {
	v := d.SelfView(r)
	if v == nil {
		return nil
	}
	out := make([]bool, len(v.Steps))
	for i, s := range v.Steps {
		out[i] = s.Done && s.Stale == 0
	}
	return out
}

// setTick ticks step n of the check at head, or takes the tick away, as SelfStep does from the board.
func (d *Dispatcher) setTick(id string, steps []core.CheckStep, n int, on bool, head string) {
	d.Store.Update(id, func(r *core.Record) {
		sc := r.SelfCheck
		if sc == nil {
			sc = &core.SelfCheck{}
		}
		ticks := selfTicks(sc, steps)
		if on {
			ticks[n] = core.SelfStep{Text: ticks[n].Text, Done: true, Commit: head, At: now()}
		} else {
			ticks[n] = core.SelfStep{Text: ticks[n].Text}
		}
		sc.Steps, sc.At, sc.Commit = ticks, "", ""
		if on && allAt(ticks, head) {
			sc.At, sc.Commit = now(), head
		}
		r.SelfCheck = sc
	})
}
