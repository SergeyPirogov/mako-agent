package dispatch

import (
	"os"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// A replayed conversation, kept while its transcript stays the same: the board asks for it every second.
type replayed struct {
	path, worktree string
	size, mod      int64
	used           time.Time
	snap           chat.Snapshot
}

// maxReplays: how many replayed conversations are kept; the board shows one task at a time.
const maxReplays = 4

// TaskChat: the conversation of a task in a Mako terminal whose agent is not running (Mako restarted, or it was
// closed), from its Claude Code transcript, so the chat shows it until a message resumes it.
func (d *Dispatcher) TaskChat(id string) (chat.Snapshot, bool) {
	r, ok := d.Store.Get(id)
	if !ok || !onTerminal(r) || r.Worktree == "" {
		return chat.Snapshot{}, false
	}
	path := orca.Transcript(r.Worktree, r.SessionID)
	if path == "" {
		return chat.Snapshot{}, false
	}
	return d.replayCached(id, r.Worktree, path)
}

// replayCached: the conversation in the transcript at path, read again only once the file changed.
func (d *Dispatcher) replayCached(id, worktree, path string) (chat.Snapshot, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return chat.Snapshot{}, false
	}
	d.replayMu.Lock()
	defer d.replayMu.Unlock()
	if d.replays == nil {
		d.replays = map[string]replayed{}
	}
	if c, ok := d.replays[id]; ok && c.path == path && c.worktree == worktree && c.size == fi.Size() && c.mod == fi.ModTime().UnixNano() {
		c.used = time.Now()
		d.replays[id] = c
		return c.snap, true
	}
	snap := chat.ReplayIn(id, worktree, path)
	// The file's size is the version: a transcript that grew by a tool's result alone has no new item, but a step
	// that finished.
	snap.Version = int(fi.Size())
	d.replays[id] = replayed{path: path, worktree: worktree, size: fi.Size(), mod: fi.ModTime().UnixNano(), used: time.Now(), snap: snap}
	d.pruneReplays()
	return snap, true
}

// TermStatus: what the bar over the agent's terminal says: what it does, its turn so far, its plan and context.
type TermStatus struct {
	State    string         `json:"state"` // "working", "waiting", "done", "exited"; "" with no agent
	Since    int64          `json:"since,omitempty"`
	Live     bool           `json:"live"`
	Tool     string         `json:"tool,omitempty"` // the tool it runs or asks for: «Bash: npm test»
	Turn     chat.TurnStat  `json:"turn"`
	Progress *chat.Progress `json:"progress,omitempty"`
	Context  int            `json:"context,omitempty"`
	Window   int            `json:"window,omitempty"`
	Model    string         `json:"model,omitempty"`
	Effort   string         `json:"effort,omitempty"`
	ViewNext string         `json:"viewNext,omitempty"`
}

// TerminalStatus: the status of a task's agent in Mako's terminal, from its hooks and its Claude Code transcript.
func (d *Dispatcher) TerminalStatus(id string) (TermStatus, bool) {
	r, ok := d.Store.Get(id)
	if !ok || !onTerminal(r) {
		return TermStatus{}, false
	}
	st := TermStatus{Model: r.AgentModel, ViewNext: r.ViewNext}
	if r.AgentRoute != nil {
		st.Effort = r.AgentRoute.Effort
		if st.Model == "" {
			st.Model = r.AgentRoute.Model
		}
	}
	if s := d.Sessions.Get(id); s != nil {
		now := s.State()
		st.State, st.Since, st.Tool, st.Live = now.State, now.Since.UnixMilli(), now.Tool, now.State != "exited"
	}
	if r.Worktree == "" {
		return st, true
	}
	path := orca.Transcript(r.Worktree, r.SessionID)
	if path == "" {
		return st, true
	}
	snap, ok := d.replayCached(id, r.Worktree, path)
	if !ok {
		return st, true
	}
	st.Turn, st.Progress, st.Context = snap.Turn, snap.Progress, snap.Context
	if st.Model == "" {
		st.Model = snap.TurnModel
	}
	// What the turn spent so far, at the list prices of the model that spent it: an estimate.
	prices := d.Config().Prices
	if prices == nil {
		prices = core.DefaultPrices()
	}
	model := snap.TurnModel
	if model == "" {
		model = st.Model
	}
	if cost := prices.Cost(model, snap.TurnUsage); cost > 0 {
		st.Turn.Cost, st.Turn.Estimate = cost, true
	}
	if st.Context > 0 {
		st.Window = core.ContextWindow(st.Model, st.Context)
	}
	return st, true
}

// pruneReplays drops the conversations of tasks gone or no longer in a terminal, and the least recently shown
// beyond maxReplays. d.replayMu is held.
func (d *Dispatcher) pruneReplays() {
	for id := range d.replays {
		if r, ok := d.Store.Get(id); !ok || !onTerminal(r) || r.Worktree == "" {
			delete(d.replays, id)
		}
	}
	for len(d.replays) > maxReplays {
		oldest := ""
		for id, c := range d.replays {
			if oldest == "" || c.used.Before(d.replays[oldest].used) {
				oldest = id
			}
		}
		delete(d.replays, oldest)
	}
}
