package dispatch

import (
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
)

// AuthHold: the Claude login failed a turn, so no new agent starts: each would fail the same way. It is lifted by
// the first turn of any agent that ends well, or by «I logged in — continue» on the board.
type AuthHold struct {
	At   int64  `json:"at"`   // when the login failed (unix seconds)
	Text string `json:"text"` // what Claude Code said
}

// AuthHold: what holds the queue now because of the login, nil when nothing does.
func (d *Dispatcher) AuthHold() *AuthHold {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authHold == nil {
		return nil
	}
	h := *d.authHold
	return &h
}

// NoteAuth is what an agent's session tells of the login: that it failed a turn (with Claude Code's words), or
// that a turn ended well after all (failed false). Told once when the queue stops and once when it goes on.
func (d *Dispatcher) NoteAuth(failed bool, text string) {
	d.mu.Lock()
	was := d.authHold != nil
	switch {
	case failed && !was:
		d.authHold = &AuthHold{At: time.Now().Unix(), Text: text}
	case failed:
		d.authHold.Text = text
	case was:
		d.authHold = nil
	}
	d.mu.Unlock()
	switch {
	case failed && !was:
		d.Log("mako", "the Claude login failed: new tasks wait until you log in again")
		notify.Inform(notify.Stopped, i18n.T("notify.authTitle"), i18n.T("notify.authText"), d.BoardURL)
	case !failed && was:
		d.Log("mako", "the Claude login works again, the queue goes on")
		d.Wake()
	}
}

// ClearAuth: «I logged in — continue» on the board. False when the queue was not held for the login.
func (d *Dispatcher) ClearAuth() bool {
	d.mu.Lock()
	was := d.authHold != nil
	d.authHold = nil
	d.mu.Unlock()
	if was {
		d.Log("mako", "the Claude login is said to be fixed, the queue goes on")
		d.Wake()
	}
	return was
}

// authReason: why the queue waits for the login, "" when it does not.
func (d *Dispatcher) authReason() string {
	if d.AuthHold() == nil {
		return ""
	}
	return "the Claude login failed: run `claude auth login`, then press «I logged in — continue» on the board"
}
