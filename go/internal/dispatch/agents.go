package dispatch

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
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
	// Terminal: the agent runs in Mako's own terminal; Live: its process is there to type into.
	Terminal bool `json:"terminal,omitempty"`
	Live     bool `json:"live,omitempty"`
	// Asked: the request a chat agent waits on, which a notification can answer.
	Asked *chat.Item `json:"-"`
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
	live := map[string]bool{}
	for _, r := range d.Store.All() {
		if r.Status != core.HandedOff {
			continue
		}
		if onTerminal(r) {
			live[r.ID] = true
			d.setAgent(r, d.lookTerminal(r, now), now)
		} else if r.Worktree != "" || branchOf(r) != "" {
			records = append(records, r)
		}
	}
	if len(records) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ws, err := d.Term.Workspaces(cctx, o)
		cancel()
		for _, r := range records {
			live[r.ID] = true
			d.setAgent(r, d.look(ctx, o, r, ws, err, now), now)
		}
	}
	d.mu.Lock()
	working := false
	for id, a := range d.agents {
		if !live[id] {
			delete(d.agents, id)
		} else if a.State == "working" {
			working = true
		}
	}
	d.mu.Unlock()
	if d.KeepAwake != nil {
		d.KeepAwake(working)
	}
}

// setAgent keeps the latest look at a task's agent and tells about a new state.
func (d *Dispatcher) setAgent(r core.Record, a Agent, now time.Time) {
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
	if s := workStage(r, a.State, d.reviewingNow(r.ID)); s != "" && r.Status == core.HandedOff {
		d.enterStage(r, s, now)
	}
	// Another request while it still waits is told too: the notification answers that one now.
	if before != nil && (before.State != a.State || (a.State == "waiting" && askedID(a.Asked) != "" && askedID(a.Asked) != askedID(before.Asked))) {
		d.tell(r, a)
	}
	// Answered (on the board, in Orca, from a notification): the notification that asked goes away.
	if before != nil && before.State == "waiting" && a.State != "waiting" {
		notify.Withdraw(waitTag(r.ID))
	}
	// A turn that ended may have opened the MR: look for it now, not at the next poll of the trackers.
	if before != nil && before.State == "working" && a.State != "working" {
		d.Wake()
	}
}

func (d *Dispatcher) tell(r core.Record, a Agent) {
	link := d.BoardURL + "/#" + r.ID
	switch a.State {
	case "waiting":
		d.Log(r.ID, "agent is waiting for you"+orBlank(": ", a.Reason))
		question := ""
		if a.Asked != nil {
			question, _, _ = chat.Choices(*a.Asked)
		}
		notify.Show(notify.Notice{Kind: notify.Waiting, Title: i18n.T("notify.waiting", r.ID), Message: firstOf(question, a.Reason, lastLine(a.Screen, ""), r.Task.Title),
			URL: link, Tag: waitTag(r.ID), Actions: askActions(r.ID, a.Asked)})
	case "done":
		d.Log(r.ID, "agent finished")
		notify.Inform(notify.Finished, i18n.T("notify.finished", r.ID), firstOf(clipText(firstLine(a.Message), 140), r.Task.Title), link)
	case "stopped":
		d.Log(r.ID, "the agent stopped"+orBlank(": ", a.Reason))
		notify.Inform(notify.Stopped, i18n.T("notify.stopped", r.ID), firstOf(a.Reason, r.Task.Title), link)
	}
}

// waitTag names the task's «agent is waiting for you» notification: a new one replaces it, an answer takes it away.
func waitTag(id string) string { return "wait:" + id }

func askedID(it *chat.Item) string {
	if it == nil {
		return ""
	}
	return it.ID
}

// askActions: the buttons that answer a chat agent's request right from the notification: «Дозволити» / «Ні»
// for a permission, the options of a one-question AskUserQuestion (up to 4; more go to the board).
func askActions(id string, it *chat.Item) []notify.Action {
	if it == nil {
		return nil
	}
	q := "id=" + url.QueryEscape(id) + "&req=" + url.QueryEscape(it.ID)
	if it.Tool == "AskUserQuestion" {
		_, options, ok := chat.Choices(*it)
		if !ok || len(options) > 4 {
			return nil
		}
		var out []notify.Action
		for i, o := range options {
			out = append(out, notify.Action{Title: o, URL: "/api/choose?" + q + "&option=" + strconv.Itoa(i)})
		}
		return out
	}
	if it.Tool == chat.PlanTool {
		// Notes on the plan need the board.
		return []notify.Action{{Title: i18n.T("action.approvePlan"), URL: "/api/plan?" + q + "&do=approve"}}
	}
	return []notify.Action{
		{Title: i18n.T("action.allow"), URL: "/api/permit?" + q + "&decision=allow"},
		{Title: i18n.T("action.deny"), URL: "/api/permit?" + q + "&decision=deny", Destructive: true},
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
		a.Error = fmt.Sprintf("Orca has no workspace %s (branch %s): it was deleted or moved", r.Worktree, branchOf(r))
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
			a.Reason = "permission for " + a.Tool
		case w.Status == "permission" || row.State == "blocked":
			a.Reason = "asks for permission"
		default:
			a.Reason = "question"
		}
	case row.State == "working":
		a.State = "working"
	case row.Interrupted || w.SidebarStatus == "interrupted" || w.SidebarStatus == "failed":
		a.State = "stopped"
		a.Reason = map[string]string{"failed": "error"}[w.SidebarStatus]
		if a.Reason == "" {
			a.Reason = "interrupted"
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
	if !ok || r.Worktree == "" || onTerminal(r) {
		return fmt.Errorf("task is not in Orca")
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
	text, err := writer.TaskSummary(d.spending(ctx, r.ID, core.PhaseWork), r.Worktree, baseBranch, r.Task, d.agentWords(ctx, r), reviewURL)
	if err != nil {
		d.Fault(r.ID, "summary not written", err)
		return
	}
	d.Store.Update(r.ID, func(r *core.Record) { r.Summary = text })
	d.Log(r.ID, "agent work summary written")
	if !o.SummaryComment {
		return
	}
	if t := d.tracker(r.Task.TrackerID); t != nil {
		if err := t.Comment(ctx, r.Task, i18n.TA("comment.summary")+"\n\n"+text); err != nil {
			d.Fault(r.ID, "summary not added to the task", err)
		}
	}
}

// agentWords: the agent's last words, from Mako's own view of it, its chat or terminal, or Orca.
func (d *Dispatcher) agentWords(ctx context.Context, r core.Record) string {
	text := ""
	d.mu.Lock()
	if a := d.agents[r.ID]; a != nil {
		text = a.Message
	}
	d.mu.Unlock()
	if s := d.Chats.Get(r.ID); s != nil && text == "" {
		text = s.LastText()
	}
	if s := d.Sessions.Get(r.ID); s != nil && text == "" {
		text = s.Output(6000)
	}
	if !onTerminal(r) {
		text = d.orcaText(ctx, d.Config().Orca, r, text)
	}
	return text
}

// orcaText: the agent's last message as Orca has it, else the end of its terminal; text when there is one already.
func (d *Dispatcher) orcaText(ctx context.Context, o config.Orca, r core.Record, text string) string {
	if text != "" {
		return text
	}
	if ws, err := d.Term.Workspaces(ctx, o); err == nil {
		if w := orca.FindWorkspace(ws, r.Worktree, branchOf(r)); w != nil && w.MainAgent() != nil && w.MainAgent().LastAssistantMessage != "" {
			return w.MainAgent().LastAssistantMessage
		}
	}
	if ts, _, err := d.Term.List(ctx, o, r.Worktree, branchOf(r)); err == nil {
		if t := orca.AgentTerminal(ts); t != nil {
			if lines, err := d.Term.Tail(ctx, o, r.Worktree, t.Handle, 300); err == nil {
				return strings.Join(lines, "\n")
			}
		}
	}
	return ""
}

func branchOf(r core.Record) string {
	if r.Branch != "" {
		return r.Branch
	}
	return r.BranchName
}

var waitReasons = map[string]string{
	"permission": "asks for permission",
}

func waitReason(w *orca.Wait) string {
	if w.Reason == "" {
		return "question or permission"
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
