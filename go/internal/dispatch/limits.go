package dispatch

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
)

// LimitHold: a Claude usage limit is used past the threshold of the settings, so the queue waits for it to reset.
type LimitHold struct {
	Window   string `json:"window"` // "5 h" or "week"
	Used     int    `json:"used"`   // percent
	Pct      int    `json:"pct"`    // the threshold
	ResetsAt int64  `json:"resetsAt"`
}

// limitHold: what holds the queue at now, nil when nothing does. A window whose reset time has passed is fresh
// again, though no agent has reported it yet.
func limitHold(l chat.Limits, pct int, now time.Time) *LimitHold {
	if pct <= 0 {
		return nil
	}
	var hold *LimitHold
	for _, w := range []struct {
		name string
		w    *chat.Window
	}{{"5 h", l.FiveHour}, {"week", l.Week}} {
		if w.w == nil || w.w.ResetsAt <= now.Unix() || w.w.Used*100 < float64(pct) {
			continue
		}
		// Both full: the queue waits for the later reset.
		if hold == nil || w.w.ResetsAt > hold.ResetsAt {
			hold = &LimitHold{Window: w.name, Used: int(w.w.Used*100 + 0.5), Pct: pct, ResetsAt: w.w.ResetsAt}
		}
	}
	return hold
}

// LimitHold: what holds the queue now, for the board.
func (d *Dispatcher) LimitHold() *LimitHold {
	return limitHold(d.Chats.Limits(), d.Config().LimitPausePct, time.Now())
}

// limitsReported: an agent told the account's limits.
func (d *Dispatcher) limitsReported(l chat.Limits) {
	d.Chats.SetLimits(l)
	d.checkLimits(time.Now())
}

// checkLimits tells once when the queue stops at the limit and once when it goes on; agents already working
// are not stopped, only warned about.
func (d *Dispatcher) checkLimits(now time.Time) *LimitHold {
	hold := limitHold(d.Chats.Limits(), d.Config().LimitPausePct, now)
	d.mu.Lock()
	was := d.limitHeld
	d.limitHeld = hold != nil
	d.mu.Unlock()
	switch {
	case hold != nil && !was:
		when := time.Unix(hold.ResetsAt, 0).Format("02.01 15:04")
		text := i18n.T("notify.limitText", hold.Window, hold.Used, when)
		if n := d.working(); n > 0 {
			text += i18n.T("notify.limitWorking", n)
		}
		d.Log("mako", text)
		notify.Inform(notify.Limits, i18n.T("notify.limitTitle"), text, d.BoardURL)
	case hold == nil && was:
		d.Log("mako", "the Claude limit has reset, the queue goes on")
		notify.Inform(notify.Limits, i18n.T("notify.limitResetTitle"), i18n.T("notify.limitResetText"), d.BoardURL)
		go d.resumeLimited(now) // the agents the limit cut go on too, not only the queue
	}
	return hold
}

// working: tasks whose agents are at work now.
func (d *Dispatcher) working() int {
	n := 0
	for _, r := range d.Store.All() {
		if r.Status == core.HandedOff {
			n++
		}
	}
	return n
}

// limitReason: why an agent whose turn the Claude limit cut is stopped.
func limitReason(until int64) string {
	if until == 0 {
		return "the Claude limit"
	}
	return "the Claude limit until " + time.Unix(until, 0).Format("15:04")
}

// SetResumeOnReset: «Продовжити після скидання» on the chat of a task, a free chat or a question: whether its
// agent goes on by itself once the limit has reset. Only this chat's: the others keep the settings'. It works
// without a running agent too (Mako restarted meanwhile): the choice is kept with the chat.
func (d *Dispatcher) SetResumeOnReset(id string, on bool) error {
	s := d.Chats.Get(id)
	if s != nil {
		s.SetResumeOnReset(on) // tells noteLimit what it is cut by now
	}
	if !d.keepLimit(id, func(until *int64, resume *bool) {
		if *until != 0 {
			*resume = on
		}
	}) && s == nil {
		return errors.New("no such chat")
	}
	return nil
}

// keepLimit changes what is kept of the limit that cut the agent of a task, a free chat or a question; false when
// there is no such one.
func (d *Dispatcher) keepLimit(id string, change func(until *int64, resume *bool)) bool {
	found := false
	switch {
	case IsFreeChat(id):
		d.setChat(id, func(c *FreeChat) { found = true; change(&c.LimitedUntil, &c.ResumeAfterLimit) })
	case IsQuestion(id):
		d.updateThreads(func(all []CodeThread) []CodeThread {
			for i := range all {
				if all[i].ID == id {
					found = true
					change(&all[i].LimitedUntil, &all[i].ResumeAfterLimit)
				}
			}
			return all
		})
	default:
		found = d.Store.UpdateIfExists(id, func(r *core.Record) { change(&r.LimitedUntil, &r.ResumeAfterLimit) })
	}
	return found
}

// limitKept: what is kept of the limit that cut the agent of id.
func (d *Dispatcher) limitKept(id string) (until int64, resume bool) {
	switch {
	case IsFreeChat(id):
		c, _ := d.freeChat(id)
		return c.LimitedUntil, c.ResumeAfterLimit
	case IsQuestion(id):
		t, _ := d.thread(id)
		return t.LimitedUntil, t.ResumeAfterLimit
	}
	r, _ := d.Store.Get(id)
	return r.LimitedUntil, r.ResumeAfterLimit
}

// noteLimit keeps what a chat's agent told of the limit: that it cut the turn (until when) and whether it goes on
// by itself after it, or that it no longer is cut (until 0). Kept, it outlives Mako's restart; the task's log says it.
func (d *Dispatcher) noteLimit(id string, until int64, resume bool) {
	if until == 0 {
		resume = false
	}
	was, wasResume := d.limitKept(id)
	if !d.keepLimit(id, func(u *int64, r *bool) { *u, *r = until, resume }) || IsFreeChat(id) || IsQuestion(id) {
		return
	}
	switch {
	case until != 0 && until != was && resume:
		d.Log(id, "the Claude limit cut the agent; it goes on by itself after "+time.Unix(until, 0).Format("15:04"))
	case until != 0 && until != was:
		d.Log(id, "the Claude limit cut the agent until "+time.Unix(until, 0).Format("15:04")+"; going on by itself is off for this chat")
	case until != 0 && resume != wasResume:
		d.Log(id, map[bool]string{true: "it goes on by itself after the Claude limit", false: "going on by itself after the Claude limit is off for this chat"}[resume])
	}
}

// WithLimit: the conversation of an agent that is not running, as the limit kept for it shows: cut, until when,
// and whether it goes on by itself. The version changes with them, so the board draws the change.
func (d *Dispatcher) WithLimit(id string, snap chat.Snapshot) chat.Snapshot {
	until, resume := d.limitKept(id)
	if until == 0 {
		return snap
	}
	snap.State, snap.LimitedUntil, snap.ResumeOnReset = "limited", until, resume
	snap.Version = snap.Version*4 + 1
	if resume {
		snap.Version += 2
	}
	return snap
}

// ForgetLimit: the agent was stopped by you: it does not go on by itself after the limit.
func (d *Dispatcher) ForgetLimit(id string) {
	if until, _ := d.limitKept(id); until != 0 {
		d.keepLimit(id, func(u *int64, r *bool) { *u, *r = 0, false })
	}
}

// resumeFlag: whether the agent Mako starts for id goes on by itself after the limit: the chat's own choice while
// the limit it was cut by is kept, else the settings'.
func (d *Dispatcher) resumeFlag(id string) bool {
	if until, resume := d.limitKept(id); until != 0 {
		return resume
	}
	return d.Config().ResumeAfterLimit
}

// cutAgent: an agent the Claude limit cut that is to go on by itself.
type cutAgent struct {
	id, name string
	task     bool
	rank     int   // the task's priority, 0 the highest
	until    int64 // when the limit reset
}

// resumeLimited makes the agents the Claude limit cut go on, once it has reset: the ones Mako was asked to (the
// settings', or the chat's own choice), the tasks first by their priority, then the chats, and no more at once
// than the WIP limit lets (the rest wait for the next round). A limit still past the threshold of the settings
// (the week's) waits as the queue does. It runs every few seconds and once Mako starts, so a restart in between
// loses nothing. It returns how many went on.
func (d *Dispatcher) resumeLimited(now time.Time) int {
	if !d.resuming.CompareAndSwap(false, true) {
		return 0 // the round before is still at it
	}
	defer d.resuming.Store(false)
	if limitHold(d.Chats.Limits(), d.Config().LimitPausePct, now) != nil || d.HoldReason() != "" {
		return 0
	}
	due := func(until int64, resume bool) bool {
		return resume && !now.Before(time.Unix(until, 0).Add(chat.ResumeGrace))
	}
	busy := func(id string) bool {
		if s := d.Chats.Get(id); s != nil {
			st, _, _ := s.State()
			return st == "working" || st == "waiting"
		}
		return false
	}
	var cut []cutAgent
	// The WIP limit is the queue's (release): tasks at work and starting. The chats and questions at work are not
	// in it, there as there; the ones made to go on here take slots of it, after the tasks.
	d.mu.Lock()
	active := len(d.starting)
	d.mu.Unlock()
	for _, r := range d.Store.All() {
		if r.LimitedUntil == 0 {
			if r.Status == core.HandedOff {
				active++
			}
			continue
		}
		if r.Status != core.HandedOff || busy(r.ID) {
			d.ForgetLimit(r.ID) // finished, cancelled, or already going on by a message of yours
			continue
		}
		if due(r.LimitedUntil, r.ResumeAfterLimit) {
			cut = append(cut, cutAgent{id: r.ID, name: r.ID, task: true, rank: priorityRank(r.Task.Priority), until: r.LimitedUntil})
		}
	}
	d.chatsMu.Lock()
	chats := d.readChats()
	d.chatsMu.Unlock()
	for _, c := range chats {
		if c.LimitedUntil != 0 && due(c.LimitedUntil, c.ResumeAfterLimit) && !busy(c.ID) {
			cut = append(cut, cutAgent{id: c.ID, name: c.Title, rank: 99, until: c.LimitedUntil})
		}
	}
	d.questionsMu.Lock()
	threads := d.readThreads()
	d.questionsMu.Unlock()
	for _, t := range threads {
		if t.LimitedUntil != 0 && due(t.LimitedUntil, t.ResumeAfterLimit) && !busy(t.ID) {
			cut = append(cut, cutAgent{id: t.ID, name: t.Title, rank: 99, until: t.LimitedUntil})
		}
	}
	sort.SliceStable(cut, func(i, j int) bool {
		if cut[i].rank != cut[j].rank {
			return cut[i].rank < cut[j].rank
		}
		return cut[i].until < cut[j].until
	})
	if wip := d.Config().WipLimit; wip > 0 {
		cut = cut[:max(0, min(len(cut), wip-active))]
	}
	var names []string
	for _, a := range cut {
		// Forgotten first: should it be cut again at once, that is the new limit to keep.
		d.ForgetLimit(a.id)
		if err := d.goOnAfterLimit(a.id); err != nil {
			d.Log(logID(a), "could not go on after the Claude limit: "+err.Error())
			d.keepFailedResume(a)
			continue
		}
		delete(d.resumeFails, a.id)
		d.Log(logID(a), "the Claude limit has reset: the agent goes on by itself")
		names = append(names, a.name)
	}
	if len(names) > 0 {
		notify.Inform(notify.Limits, i18n.T("notify.limitContinuedTitle"), i18n.T("notify.limitContinuedText", len(names), strings.Join(names, ", ")), d.BoardURL)
	}
	return len(names)
}

// maxResumeTries: how many rounds an agent that could not be made to go on is tried again before Mako leaves it
// to you (its Resume button); a worktree that is gone or a task outside Mako's terminal will not get better.
const maxResumeTries = 5

// keepFailedResume: the message did not reach the agent, so it is still cut: the limit is kept for the next round
// (a moment's failure, a worktree being made), unless it failed maxResumeTries times in a row. Not when a new
// limit was kept meanwhile (it was cut again), or the agent is no longer to be resumed.
func (d *Dispatcher) keepFailedResume(a cutAgent) {
	if d.resumeFails == nil {
		d.resumeFails = map[string]int{}
	}
	d.resumeFails[a.id]++
	if d.resumeFails[a.id] >= maxResumeTries {
		delete(d.resumeFails, a.id)
		d.Log(logID(a), "giving up going on by itself after the Claude limit: resume it yourself")
		return
	}
	d.keepLimit(a.id, func(u *int64, r *bool) {
		if *u == 0 {
			*u, *r = a.until, true
		}
	})
}

// logID: whose log tells of the agent: the task's own, else Mako's.
func logID(a cutAgent) string {
	if a.task {
		return a.id
	}
	return "mako"
}

// goOnAfterLimit says «go on» to the agent the way your message from the board goes: into its conversation, or
// resuming a closed one (Mako restarted, or it slept).
func (d *Dispatcher) goOnAfterLimit(id string) error {
	if d.goOn != nil {
		return d.goOn(id)
	}
	switch {
	case IsFreeChat(id):
		return d.SayFree(id, chat.ResumeMessage)
	case IsQuestion(id):
		t, ok := d.thread(id)
		if !ok {
			return errors.New("no such question")
		}
		_, err := d.AskCode(t.Project, id, chat.ResumeMessage)
		return err
	}
	return d.say(id, chat.ResumeMessage)
}

// priorityRank: how urgent a tracker's priority is, 0 the most ("Blocker", "P0") to 4 the least ("Lowest", "P4");
// none, or one Mako does not know, is in the middle.
func priorityRank(p string) int {
	p = strings.ToLower(strings.TrimSpace(p))
	if len(p) == 2 && p[0] == 'p' && p[1] >= '0' && p[1] <= '4' {
		return int(p[1] - '0')
	}
	switch {
	case strings.Contains(p, "highest"), strings.Contains(p, "blocker"), strings.Contains(p, "critical"), strings.Contains(p, "urgent"):
		return 0
	case strings.Contains(p, "lowest"), strings.Contains(p, "trivial"):
		return 4
	case strings.Contains(p, "high"), strings.Contains(p, "major"):
		return 1
	case strings.Contains(p, "low"), strings.Contains(p, "minor"):
		return 3
	}
	return 2
}
