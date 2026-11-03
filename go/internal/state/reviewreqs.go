package state

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
)

// ReviewRequests keeps the merge requests waiting for my review in review-requests.json, apart from the
// tasks' state.json (the TypeScript Mako reads that one).
type ReviewRequests struct {
	mu   sync.Mutex
	file string
	list map[string]*core.ReviewRequest
	// OnSaveError hears that a change stayed in memory only; it is called outside the lock.
	OnSaveError func(error)
}

func OpenReviewRequests(dir string) (*ReviewRequests, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &ReviewRequests{file: filepath.Join(dir, "review-requests.json"), list: map[string]*core.ReviewRequest{}}
	if data, err := os.ReadFile(s.file); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s.list); err != nil {
			// A broken file must not turn the feature off: it is kept aside and the list starts anew (the
			// next search fills it again).
			if to, rerr := keepAside(s.file); rerr != nil {
				log.Printf("review-requests.json: cannot be read (%v) nor kept aside: %v", err, rerr)
			} else {
				log.Printf("review-requests.json: cannot be read (%v): kept as %s", err, to)
			}
			s.list = map[string]*core.ReviewRequest{}
		}
	}
	return s, nil
}

func (s *ReviewRequests) Get(id string) (core.ReviewRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.list[id]
	if !ok {
		return core.ReviewRequest{}, false
	}
	return *r, true
}

// All: copies, the longest waiting first.
func (s *ReviewRequests) All() []core.ReviewRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.ReviewRequest, 0, len(s.list))
	for _, r := range s.list {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Since < out[j].Since || out[i].Since == out[j].Since && out[i].ID < out[j].ID
	})
	return out
}

// Update changes (or makes) the request under the lock and saves.
func (s *ReviewRequests) Update(id string, change func(r *core.ReviewRequest)) core.ReviewRequest {
	out, err := s.update(id, change)
	s.saveFailed(err)
	return out
}

func (s *ReviewRequests) update(id string, change func(r *core.ReviewRequest)) (core.ReviewRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.list[id]
	if !ok {
		r = &core.ReviewRequest{ID: id}
		s.list[id] = r
	}
	change(r)
	r.ID = id
	return *r, s.saveLocked()
}

func (s *ReviewRequests) Delete(id string) {
	s.mu.Lock()
	var err error
	if _, ok := s.list[id]; ok {
		delete(s.list, id)
		err = s.saveLocked()
	}
	s.mu.Unlock()
	s.saveFailed(err)
}

func (s *ReviewRequests) saveFailed(err error) {
	if err == nil {
		return
	}
	log.Printf("review-requests.json: not saved: %v", err)
	if s.OnSaveError != nil {
		s.OnSaveError(err)
	}
}

func (s *ReviewRequests) saveLocked() error {
	data, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFile(s.file, data, 0o600)
}
