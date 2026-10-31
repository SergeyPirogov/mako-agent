package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Terminals is how Mako sees the agents' terminals in Orca; tests replace it.
type Terminals interface {
	List(ctx context.Context, o config.Orca, worktree string) ([]orca.Terminal, error)
	Waiting(ctx context.Context, o config.Orca, worktree, handle string) (*orca.Wait, error)
	Screen(ctx context.Context, o config.Orca, worktree, handle string) ([]string, error)
	Tail(ctx context.Context, o config.Orca, worktree, handle string, lines int) ([]string, error)
	Focus(ctx context.Context, o config.Orca, worktree, handle string) error
}

type orcaTerminals struct{}

func (orcaTerminals) List(ctx context.Context, o config.Orca, wt string) ([]orca.Terminal, error) {
	return orca.Terminals(ctx, o, wt)
}
func (orcaTerminals) Waiting(ctx context.Context, o config.Orca, wt, h string) (*orca.Wait, error) {
	return orca.Waiting(ctx, o, wt, h)
}
func (orcaTerminals) Screen(ctx context.Context, o config.Orca, wt, h string) ([]string, error) {
	return orca.Screen(ctx, o, wt, h)
}
func (orcaTerminals) Tail(ctx context.Context, o config.Orca, wt, h string, n int) ([]string, error) {
	return orca.Tail(ctx, o, wt, h, n)
}
func (orcaTerminals) Focus(ctx context.Context, o config.Orca, wt, h string) error {
	return orca.Focus(ctx, o, wt, h)
}

// Agent is what the agent of a task in Orca is doing, as last seen.
type Agent struct {
	State  string   `json:"state"` // "working", "waiting" (for you), "quiet" (stopped talking), "gone", "unknown"
	Reason string   `json:"reason,omitempty"`
	Since  int64    `json:"since,omitempty"` // ms: when it came to this state
	Name   string   `json:"name,omitempty"`  // "claude", "codex", …
	Screen []string `json:"screen,omitempty"`
	Error  string   `json:"error,omitempty"`
	At     int64    `json:"at"`
	handle string
}

// QuietAfter: an agent without output this long has stopped: done, or waiting in a way Orca cannot see.
const QuietAfter = 90 * time.Second

// Agents: the latest look at every task in Orca.
func (d *Dispatcher) Agents() map[string]Agent {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]Agent{}
	for id, a := range d.agents {
		out[id] = *a
	}
	return out
}

// WatchAgents looks into the terminal of every task in Orca: its screen, and whether the agent waits for you.
// Coming to «waiting» or going quiet is told once, with a notification.
func (d *Dispatcher) WatchAgents(ctx context.Context, now time.Time) {
	o := d.Config().Orca
	live := map[string]bool{}
	for _, r := range d.Store.All() {
		if r.Status != core.HandedOff || r.Worktree == "" {
			continue
		}
		live[r.ID] = true
		a := d.look(ctx, o, r, now)
		d.mu.Lock()
		before := d.agents[r.ID]
		if before != nil && before.State == a.State {
			a.Since = before.Since
		} else if a.Since == 0 {
			a.Since = now.UnixMilli()
		}
		d.agents[r.ID] = &a
		d.mu.Unlock()
		if before != nil && before.State != a.State {
			switch a.State {
			case "waiting":
				d.Log(r.ID, "агент чекає на вас"+orBlank(": ", a.Reason))
				notify.Inform("✋ "+r.ID+": агент чекає на вас", lastLine(a.Screen, r.Task.Title), d.BoardURL+"/#"+r.ID)
			case "quiet":
				d.Log(r.ID, "агент замовк")
				notify.Inform("💤 "+r.ID+": агент зупинився", "Подивіться, чи він закінчив або чекає відповіді. "+lastLine(a.Screen, ""), d.BoardURL+"/#"+r.ID)
			}
		}
	}
	d.mu.Lock()
	for id := range d.agents {
		if !live[id] {
			delete(d.agents, id)
		}
	}
	d.mu.Unlock()
}

func (d *Dispatcher) look(ctx context.Context, o config.Orca, r core.Record, now time.Time) Agent {
	a := Agent{State: "unknown", At: now.UnixMilli()}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ts, err := d.Term.List(ctx, o, r.Worktree)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	t := orca.AgentTerminal(ts)
	if t == nil {
		a.State = "gone"
		return a
	}
	a.Name, a.handle = t.Agent, t.Handle
	if t.ExitCause != nil {
		a.State = "gone"
	} else if w, err := d.Term.Waiting(ctx, o, r.Worktree, t.Handle); err == nil && w != nil {
		a.State, a.Reason, a.Since = "waiting", waitReason(w), w.Since
	} else if t.LastOutputAt > 0 && now.Sub(time.UnixMilli(t.LastOutputAt)) > QuietAfter {
		a.State, a.Since = "quiet", t.LastOutputAt
	} else {
		a.State = "working"
	}
	if screen, err := d.Term.Screen(ctx, o, r.Worktree, t.Handle); err == nil {
		a.Screen = trimScreen(screen)
	} else if t.Preview != "" {
		a.Screen = trimScreen(strings.Split(t.Preview, "\n"))
	}
	return a
}

// FocusAgent brings the task's terminal forward in Orca.
func (d *Dispatcher) FocusAgent(ctx context.Context, id string) error {
	r, ok := d.Store.Get(id)
	if !ok || r.Worktree == "" {
		return fmt.Errorf("задача не в Orca")
	}
	o := d.Config().Orca
	ts, err := d.Term.List(ctx, o, r.Worktree)
	if err != nil {
		return err
	}
	t := orca.AgentTerminal(ts)
	if t == nil {
		return fmt.Errorf("термінала агента вже немає")
	}
	return d.Term.Focus(ctx, o, r.Worktree, t.Handle)
}

// summarize writes what the agent did, once its MR appeared: from the end of its terminal session and the
// branch's changes. It goes to the card, and with orca.summaryComment to the issue too.
func (d *Dispatcher) summarize(ctx context.Context, r core.Record, baseBranch, reviewURL string) {
	if r.Worktree == "" || !exists(r.Worktree) {
		return
	}
	o := d.Config().Orca
	agentText := ""
	if ts, err := d.Term.List(ctx, o, r.Worktree); err == nil {
		if t := orca.AgentTerminal(ts); t != nil {
			if lines, err := d.Term.Tail(ctx, o, r.Worktree, t.Handle, 300); err == nil {
				agentText = strings.Join(lines, "\n")
			}
		}
	}
	text, err := writer.TaskSummary(ctx, r.Worktree, baseBranch, r.Task, agentText, reviewURL)
	if err != nil {
		d.Log(r.ID, "підсумок не написано: "+err.Error())
		return
	}
	d.Store.Update(r.ID, func(r *core.Record) { r.Summary = text })
	d.Log(r.ID, "підсумок роботи агента написано")
	if !o.SummaryComment {
		return
	}
	if t := d.tracker(r.Task.TrackerID); t != nil {
		if err := t.Comment(ctx, r.Task, "🦈 Mako: що зробив агент\n\n"+text); err != nil {
			d.Log(r.ID, "підсумок у задачу не додано: "+err.Error())
		}
	}
}

var waitReasons = map[string]string{
	"permission": "просить дозволу",
}

func waitReason(w *orca.Wait) string {
	if w.Reason == "" {
		return "питання або дозвіл"
	}
	if r, ok := waitReasons[w.Reason]; ok {
		return r
	}
	return strings.ReplaceAll(w.Reason, "-", " ")
}

// trimScreen drops the blank lines at the end and keeps the last 40.
func trimScreen(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	lines = lines[:end]
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(l, " ")
	}
	return out
}

func lastLine(screen []string, fallback string) string {
	for i := len(screen) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(strings.Trim(screen[i], "│╭╮╰╯─ >")); len([]rune(l)) > 3 {
			return clipText(l, 120)
		}
	}
	return fallback
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func orBlank(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}
