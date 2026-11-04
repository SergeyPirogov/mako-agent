// Package state keeps Mako's records in state.json, the same file the TypeScript Mako uses.
package state

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
)

type Store struct {
	mu      sync.Mutex
	file    string
	records map[string]*core.Record
	// Broken: where a state.json that could not be read was kept aside ("" when it was fine).
	Broken string
	// OnSaveError hears that a change stayed in memory only (a full disk, no rights); it is called outside the lock.
	OnSaveError func(error)
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{file: filepath.Join(dir, "state.json"), records: map[string]*core.Record{}}
	data, err := os.ReadFile(s.file)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s.records); err != nil {
			// A broken file must not keep Mako from starting: it is kept aside and the board starts empty.
			s.records = map[string]*core.Record{}
			broken, rerr := keepAside(s.file)
			if rerr != nil {
				return nil, fmt.Errorf("%w; and it cannot be kept aside: %v", err, rerr)
			}
			s.Broken = broken
			log.Printf("state.json: CANNOT BE READ (%v): kept as %s, Mako starts with an empty board", err, s.Broken)
		}
	}
	// Records from before ErrorKind: the cause is told from the text, in either language.
	for _, r := range s.records {
		if r.Error != "" && r.ErrorKind == "" {
			r.ErrorKind = core.ErrorKindFromText(r.Error)
		}
	}
	// A notification left on screen when Mako stopped: ask again.
	for _, r := range s.records {
		if r.Status == core.Asking {
			r.Status, r.SnoozedUntil = core.Snoozed, 0
		}
	}
	return s, s.saveLocked()
}

// keepAside moves a file that cannot be read to file.broken, or to file.broken-<time> when an earlier one is
// already there, so no broken copy is ever written over. It returns where the file went.
func keepAside(file string) (string, error) {
	to := file + ".broken"
	if _, err := os.Lstat(to); err == nil {
		to = file + ".broken-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	return to, os.Rename(file, to)
}

// Get returns a deep copy, so a caller cannot change a record without Update, and an Update changing the stored
// one does not race with the caller reading its copy.
func (s *Store) Get(id string) (core.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return core.Record{}, false
	}
	return r.Clone(), true
}

// All returns deep copies of every record, the most recently updated first.
func (s *Store) All() []core.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// changeKeepingKind applies a change and, when it set a new Error without its ErrorKind, tells the kind from the
// text (and drops the old kind when the error went), so the kind never stays from an earlier failure and the
// board and the statistics read the same cause.
func changeKeepingKind(r *core.Record, change func(r *core.Record)) {
	was, kind := r.Error, r.ErrorKind
	change(r)
	if r.Error != was && r.ErrorKind == kind {
		r.ErrorKind = core.ErrorKindFromText(r.Error)
	}
}

// Update changes a record (or makes it) under the lock and saves.
func (s *Store) Update(id string, change func(r *core.Record)) core.Record {
	out, err := s.update(id, change)
	s.saveFailed(err)
	return out
}

func (s *Store) update(id string, change func(r *core.Record)) (core.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		r = &core.Record{ID: id, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		s.records[id] = r
	}
	changeKeepingKind(r, change)
	r.ID = id
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return r.Clone(), s.saveLocked()
}

// UpdateIfExists is Update for a record that must not come back: a background job that finishes after the task
// was deleted would otherwise leave an empty record. It reports whether the record was there.
func (s *Store) UpdateIfExists(id string, change func(r *core.Record)) bool {
	s.mu.Lock()
	r, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	changeKeepingKind(r, change)
	r.ID = id
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	err := s.saveLocked()
	s.mu.Unlock()
	s.saveFailed(err)
	return true
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	delete(s.records, id)
	err := s.saveLocked()
	s.mu.Unlock()
	s.saveFailed(err)
}

// saveFailed says loudly that the disk no longer matches what Mako keeps in memory.
func (s *Store) saveFailed(err error) {
	if err == nil {
		return
	}
	log.Printf("state.json: not saved: %v", err)
	if s.OnSaveError != nil {
		s.OnSaveError(err)
	}
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFile(s.file, data, 0o600)
}
