package dispatch

import (
	"os"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
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
	path := orca.LatestTranscript(r.Worktree)
	if path == "" {
		return chat.Snapshot{}, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return chat.Snapshot{}, false
	}
	d.replayMu.Lock()
	defer d.replayMu.Unlock()
	if d.replays == nil {
		d.replays = map[string]replayed{}
	}
	if c, ok := d.replays[id]; ok && c.path == path && c.worktree == r.Worktree && c.size == fi.Size() && c.mod == fi.ModTime().UnixNano() {
		c.used = time.Now()
		d.replays[id] = c
		return c.snap, true
	}
	snap := chat.ReplayIn(id, r.Worktree, path)
	// The file's size is the version: a transcript that grew by a tool's result alone has no new item, but a step
	// that finished.
	snap.Version = int(fi.Size())
	d.replays[id] = replayed{path: path, worktree: r.Worktree, size: fi.Size(), mod: fi.ModTime().UnixNano(), used: time.Now(), snap: snap}
	d.pruneReplays()
	return snap, true
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
