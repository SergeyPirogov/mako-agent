package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
)

// absence is when you last looked at the board and when the morning digest last went out, kept in digest.json
// so a restart of Mako does not forget them.
type absence struct {
	mu      sync.Mutex
	loaded  bool
	file    string
	saved   int64 // ms: Seen last written
	Seen    int64 `json:"seen"`    // ms: the board was last open and visible
	Morning int64 `json:"morning"` // ms: the last morning digest
}

// The board is open all day: what it saw is written once a minute, not at every look.
const seenSaveEvery = time.Minute

func (a *absence) load(dir string) {
	if a.loaded {
		return
	}
	a.loaded = true
	if dir == "" {
		return // no state folder (a test): kept in memory only
	}
	a.file = filepath.Join(dir, "digest.json")
	if data, err := os.ReadFile(a.file); err == nil {
		_ = json.Unmarshal(data, a)
	}
}

func (a *absence) save() {
	a.saved = a.Seen
	if a.file == "" {
		return
	}
	data, _ := json.Marshal(a)
	if os.WriteFile(a.file+".tmp", data, 0o600) == nil {
		_ = os.Rename(a.file+".tmp", a.file)
	}
}

// Seen: the board is open and visible now. Away for AwayHours or more (the Mac slept, the board was closed),
// it returns what happened since; nil when you were not away or nothing happened.
func (d *Dispatcher) Seen(now time.Time) *core.Digest {
	cfg := d.Config()
	a := &d.absence
	a.mu.Lock()
	a.load(cfg.StateDir)
	prev := a.Seen
	a.Seen = now.UnixMilli()
	if now.Sub(time.UnixMilli(a.saved)) >= seenSaveEvery {
		a.save()
	}
	a.mu.Unlock()
	away := time.Duration(cfg.Notifications.AwayHours) * time.Hour
	if prev == 0 || away <= 0 || now.Sub(time.UnixMilli(prev)) < away {
		return nil
	}
	dg := d.Digest(time.UnixMilli(prev), now)
	if dg.Count() == 0 {
		return nil
	}
	return &dg
}

// Digest: what waits for you now, and what failed and got merged from since to now.
func (d *Dispatcher) Digest(since, now time.Time) core.Digest {
	waiting := map[string]string{}
	all := d.Store.All()
	byID := map[string]core.Record{}
	for _, r := range all {
		byID[r.ID] = r
	}
	for id, a := range d.Agents() {
		switch a.State {
		case "waiting":
			waiting[id] = firstOf(a.Reason, "агент чекає на вас")
		case "done":
			if byID[id].PRURL == "" {
				waiting[id] = firstOf(clipText(firstLine(a.Message), 140), "агент закінчив: ваш хід")
			}
		case "stopped":
			waiting[id] = firstOf(a.Reason, "агент зупинився")
		}
	}
	return core.BuildDigest(all, waiting, since, now)
}

// morningLate: a Mac that slept through the morning digest's time gets it on waking, but not this much later.
const morningLate = 6 * time.Hour

// morning sends the morning digest once a day at its time: one notification (and one push to the phone) with
// what happened overnight. Not when you are at the board already.
func (d *Dispatcher) morning(now time.Time) {
	cfg := d.Config()
	at := cfg.Notifications.MorningMin
	if at < 0 {
		return
	}
	y, m, day := now.Date()
	due := time.Date(y, m, day, at/60, at%60, 0, 0, now.Location())
	a := &d.absence
	a.mu.Lock()
	a.load(cfg.StateDir)
	if now.Before(due) || a.Morning >= due.UnixMilli() {
		a.mu.Unlock()
		return
	}
	since := now.Add(-24 * time.Hour).UnixMilli()
	since = max(since, a.Morning, a.Seen)
	atBoard := a.Seen > 0 && now.Sub(time.UnixMilli(a.Seen)) < 30*time.Minute
	a.Morning = now.UnixMilli()
	a.save()
	a.mu.Unlock()
	if atBoard || now.Sub(due) > morningLate {
		return
	}
	dg := d.Digest(time.UnixMilli(since), now)
	if dg.Count() == 0 {
		return
	}
	notify.Inform("", "☀️ Підсумок за ніч", dg.Line(), d.BoardURL+"/#digest:"+strconv.FormatInt(since, 10))
}
