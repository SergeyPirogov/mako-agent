package dispatch

import (
	"os"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// A replayed conversation, kept while its transcript stays the same: the board asks for it every second.
type replayed struct {
	path, worktree string
	size           int64
	mod            int64
	snap           chat.Snapshot
}

var (
	replayMu sync.Mutex
	replays  = map[string]replayed{}
)

// TaskChat: the conversation of a task in a Mako terminal whose agent is not running (Mako restarted, or it was
// closed), from its Claude Code transcript, so the chat shows it until a message resumes it.
func (d *Dispatcher) TaskChat(id string) (chat.Snapshot, bool) {
	r, ok := d.Store.Get(id)
	if !ok || !onTerminal(r) || r.Worktree == "" {
		return chat.Snapshot{}, false
	}
	path := orca.LatestTranscript(r.Worktree)
	if path == "" {
		return chat.Snapshot{}, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return chat.Snapshot{}, false
	}
	replayMu.Lock()
	defer replayMu.Unlock()
	if c, ok := replays[id]; ok && c.path == path && c.worktree == r.Worktree && c.size == fi.Size() && c.mod == fi.ModTime().UnixNano() {
		return c.snap, true
	}
	snap := chat.ReplayIn(id, r.Worktree, path)
	replays[id] = replayed{path: path, worktree: r.Worktree, size: fi.Size(), mod: fi.ModTime().UnixNano(), snap: snap}
	return snap, true
}
