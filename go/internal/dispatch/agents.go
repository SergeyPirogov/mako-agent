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

// Terminals is how Mako sees the agents in Orca; tests replace it.
type Terminals interface {
	Workspaces(ctx context.Context, o config.Orca) ([]orca.Workspace, error)
	List(ctx context.Context, o config.Orca, worktree, branch string) (mine []orca.Terminal, all int, err error)
	Screen(ctx context.Context, o config.Orca, worktree, handle string) ([]string, error)
	Tail(ctx context.Context, o config.Orca, worktree, handle string, lines int) ([]string, error)
	Focus(ctx context.Context, o config.Orca, worktree, handle string) error
}

type orcaTerminals struct{}

func (orcaTerminals) Workspaces(ctx context.Context, o config.Orca) ([]orca.Workspace, error) {
	return orca.Workspaces(ctx, o)
}
func (orcaTerminals) List(ctx context.Context, o config.Orca, wt, branch string) ([]orca.Terminal, int, error) {
	return orca.Terminals(ctx, o, wt, branch)
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

// Agent is what the agent of a task in Orca is doing, as Orca's sidebar last showed it.
type Agent struct {
	// "working"; "waiting" (for you: a permission or a question); "done" (finished its turn); "stopped"
	// (interrupted or failed); "idle" (the workspace has no agent running); "missing" (Orca has no such workspace);
	// "unknown" (Orca did not answer).
	State   string   `json:"state"`
	Reason  string   `json:"reason,omitempty"`
	Since   int64    `json:"since,omitempty"` // ms: when it came to this state
	Name    string   `json:"name,omitempty"`  // "claude", "codex", …
	Tool    string   `json:"tool,omitempty"`  // what it is doing now: "Bash: npm test"
	Message string   `json:"message,omitempty"`
	Screen  []string `json:"screen,omitempty"` // a terminal agent's screen
	Error   string   `json:"error,omitempty"`
	At      int64    `json:"at"`
}

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

// WatchAgents asks Orca once (`orca worktree ps`) how the agent of every task in Orca is doing. Coming to
// «waiting», «done» or «stopped» is told once, with a notification.
func (d *Dispatcher) WatchAgents(ctx context.Context, now time.Time) {
	o := d.Config().Orca
	var records []core.Record
	for _, r := range d.Store.All() {
		if r.Status == core.HandedOff && (r.Worktree != "" || branchOf(r) != "") {
			records = append(records, r)
		}
	}
	live := map[string]bool{}
	if len(records) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ws, err := d.Term.Workspaces(cctx, o)
		cancel()
		for _, r := range records {
			live[r.ID] = true
			a := d.look(ctx, o, r, ws, err, now)
			d.mu.Lock()
			before := d.agents[r.ID]
			if a.Since == 0 {
				a.Since = now.UnixMilli()
				if before != nil && before.State == a.State {
					a.Since = before.Since
				}
			}
			d.agents[r.ID] = &a
			d.mu.Unlock()
			if before != nil && before.State != a.State {
				d.tell(r, a)
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

func (d *Dispatcher) tell(r core.Record, a Agent) {
	link := d.BoardURL + "/#" + r.ID
	switch a.State {
	case "waiting":
		d.Log(r.ID, "агент чекає на вас"+orBlank(": ", a.Reason))
		notify.Inform("✋ "+r.ID+": агент чекає на вас", firstOf(a.Reason, lastLine(a.Screen, ""), r.Task.Title), link)
	case "done":
		d.Log(r.ID, "агент закінчив")
		notify.Inform("✅ "+r.ID+": агент закінчив", firstOf(clipText(firstLine(a.Message), 140), r.Task.Title), link)
	case "stopped":
		d.Log(r.ID, "агент зупинився"+orBlank(": ", a.Reason))
		notify.Inform("⏹ "+r.ID+": агент зупинився", firstOf(a.Reason, r.Task.Title), link)
	}
}

func (d *Dispatcher) look(ctx context.Context, o config.Orca, r core.Record, ws []orca.Workspace, wsErr error, now time.Time) Agent {
	a := Agent{State: "unknown", At: now.UnixMilli()}
	if wsErr != nil {
		a.Error = wsErr.Error()
		return a
	}
	w := orca.FindWorkspace(ws, r.Worktree, branchOf(r))
	if w == nil {
		a.State = "missing"
		a.Error = fmt.Sprintf("В Orca немає workspace %s (гілка %s): його видалили або перенесли", r.Worktree, branchOf(r))
		return a
	}
	row := w.MainAgent()
	if row == nil {
		a.State = "idle"
		if w.Status == "working" {
			a.State = "working"
		}
		a.Message = strings.TrimSpace(w.Preview)
		return a
	}
	a.Name, a.Message, a.Since = row.AgentType, strings.TrimSpace(row.LastAssistantMessage), row.StateStartedAt
	if (row.AgentType == "" || row.AgentType == "claude") && d.LastMessage != nil && r.Worktree != "" {
		if full := d.LastMessage(r.Worktree); full != "" {
			a.Message = full
		}
	}
	if row.ToolName != "" {
		a.Tool = row.ToolName + orBlank(": ", clipText(firstLine(row.ToolInput), 160))
	}
	switch {
	case w.Status == "permission" || row.State == "blocked" || row.State == "waiting" || row.AgentWait != nil:
		a.State = "waiting"
		switch {
		case row.AgentWait != nil && row.AgentWait.Reason != "" && row.AgentWait.Reason != "permission":
			a.Reason = waitReason(row.AgentWait)
		case a.Tool != "":
			a.Reason = "дозвіл на " + a.Tool
		case w.Status == "permission" || row.State == "blocked":
			a.Reason = "просить дозволу"
		default:
			a.Reason = "питання"
		}
	case row.State == "working":
		a.State = "working"
	case row.Interrupted || w.SidebarStatus == "interrupted" || w.SidebarStatus == "failed":
		a.State = "stopped"
		a.Reason = map[string]string{"failed": "помилка"}[w.SidebarStatus]
		if a.Reason == "" {
			a.Reason = "перервано"
		}
	default: // "done"
		a.State, a.Tool = "done", ""
	}
	// An agent in a terminal also shows its screen.
	if row.HasLivePty {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if ts, _, err := d.Term.List(cctx, o, r.Worktree, branchOf(r)); err == nil {
			if t := orca.AgentTerminal(ts); t != nil {
				if screen, err := d.Term.Screen(cctx, o, r.Worktree, t.Handle); err == nil {
					a.Screen = trimScreen(screen)
				}
			}
		}
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
	ts, _, err := d.Term.List(ctx, o, r.Worktree, branchOf(r))
	if err != nil {
		return err
	}
	if t := orca.AgentTerminal(ts); t != nil {
		return d.Term.Focus(ctx, o, r.Worktree, t.Handle)
	}
	// An agent in Orca's chat view has no terminal to switch to: bring Orca forward at least.
	return orca.Open(ctx, o)
}

// summarize writes what the agent did, once its MR appeared: from the end of its terminal session and the
// branch's changes. It goes to the card, and with orca.summaryComment to the issue too.
func (d *Dispatcher) summarize(ctx context.Context, r core.Record, baseBranch, reviewURL string) {
	if r.Worktree == "" || !exists(r.Worktree) {
		return
	}
	o := d.Config().Orca
	agentText := ""
	d.mu.Lock()
	if a := d.agents[r.ID]; a != nil {
		agentText = a.Message
	}
	d.mu.Unlock()
	if ws, err := d.Term.Workspaces(ctx, o); err == nil && agentText == "" {
		if w := orca.FindWorkspace(ws, r.Worktree, branchOf(r)); w != nil && w.MainAgent() != nil {
			agentText = w.MainAgent().LastAssistantMessage
		}
	}
	if ts, _, err := d.Term.List(ctx, o, r.Worktree, branchOf(r)); err == nil && agentText == "" {
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
		if err := t.Comment(ctx, r.Task, "Що зроблено:\n\n"+text); err != nil {
			d.Log(r.ID, "підсумок у задачу не додано: "+err.Error())
		}
	}
}

func branchOf(r core.Record) string {
	if r.Branch != "" {
		return r.Branch
	}
	return r.BranchName
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

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(l, "#*- ")); l != "" {
			return l
		}
	}
	return ""
}

func firstOf(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
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
