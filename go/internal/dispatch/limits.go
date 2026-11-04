package dispatch

import (
	"errors"
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

// afterLimit: what a chat's agent does once the Claude limit that cut its turn has reset and it was asked to go on
// by itself: say gives it the message the way yours goes (resuming a closed agent); then you are told. name is
// what the notification calls the chat.
func (d *Dispatcher) afterLimit(name string, say func(text string) error) func(string) error {
	return func(text string) error {
		if err := say(text); err != nil {
			return err
		}
		d.Log("mako", "the Claude limit has reset, the agent of "+name+" goes on")
		notify.Inform(notify.Limits, i18n.T("notify.limitContinuedTitle"), i18n.T("notify.limitContinuedText", name), d.BoardURL)
		return nil
	}
}

// limitReason: why an agent whose turn the Claude limit cut is stopped.
func limitReason(until int64) string {
	if until == 0 {
		return "the Claude limit"
	}
	return "the Claude limit until " + time.Unix(until, 0).Format("15:04")
}

// SetResumeOnReset: «Продовжити після скидання» on the chat of a task, a free chat or a question: whether its
// agent goes on by itself once the limit has reset.
func (d *Dispatcher) SetResumeOnReset(id string, on bool) error {
	s := d.Chats.Get(id)
	if s == nil {
		return errors.New("the agent is not running: Mako was restarted, write to it to continue the conversation")
	}
	s.SetResumeOnReset(on)
	return nil
}
