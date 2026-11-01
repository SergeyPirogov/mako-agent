package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// ReviewRequests keeps the merge requests waiting for my review in review-requests.json, apart from the
// tasks' state.json (the TypeScript Mako reads that one).
type ReviewRequests struct {
	mu   sync.Mutex
	file string
	list map[string]*core.ReviewRequest
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
			_ = os.Rename(s.file, s.file+".broken")
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
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.list[id]
	if !ok {
		r = &core.ReviewRequest{ID: id}
		s.list[id] = r
	}
	change(r)
	r.ID = id
	_ = s.saveLocked()
	return *r
}

func (s *ReviewRequests) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.list[id]; ok {
		delete(s.list, id)
		_ = s.saveLocked()
	}
}

func (s *ReviewRequests) saveLocked() error {
	data, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}
