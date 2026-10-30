// Package state keeps Mako's records in state.json, the same file the TypeScript Mako uses.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

type Store struct {
	mu      sync.Mutex
	file    string
	records map[string]*core.Record
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{file: filepath.Join(dir, "state.json"), records: map[string]*core.Record{}}
	data, err := os.ReadFile(s.file)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s.records); err != nil {
			return nil, err
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

// Get returns a copy, so a caller cannot change a record without Update.
func (s *Store) Get(id string) (core.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return core.Record{}, false
	}
	return *r, true
}

// All returns copies of every record, the most recently updated first.
func (s *Store) All() []core.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// Update changes a record (or makes it) under the lock and saves.
func (s *Store) Update(id string, change func(r *core.Record)) core.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		r = &core.Record{ID: id}
		s.records[id] = r
	}
	change(r)
	r.ID = id
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.saveLocked()
	return *r
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	_ = s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}
