package state

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
)

// maxSpends: the most spends kept; the oldest go first.
const maxSpends = 2000

// Spends keeps what the agents' calls of no task cost (the reviewer over someone else's MR) in a file of
// their own: the MRs to review are forgotten a week after they are done, their cost stays in the statistics.
type Spends struct {
	mu   sync.Mutex
	file string
	list []core.Spend
}

// OpenSpends: the spends kept in dir/name; a broken file is kept aside and the list starts anew.
func OpenSpends(dir, name string) (*Spends, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Spends{file: filepath.Join(dir, name)}
	if data, err := os.ReadFile(s.file); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s.list); err != nil {
			if to, rerr := keepAside(s.file); rerr != nil {
				log.Printf("%s: cannot be read (%v) nor kept aside: %v", name, err, rerr)
			} else {
				log.Printf("%s: cannot be read (%v): kept as %s", name, err, to)
			}
			s.list = nil
		}
	}
	return s, nil
}

// Add keeps the spend and saves; one of no cost and no tokens is not kept.
func (s *Spends) Add(sp core.Spend) error {
	if sp.USD <= 0 && sp.Usage.Total() <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, sp)
	if len(s.list) > maxSpends {
		s.list = append([]core.Spend(nil), s.list[len(s.list)-maxSpends:]...)
	}
	data, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFile(s.file, data, 0o600)
}

// All: a copy, the oldest first.
func (s *Spends) All() []core.Spend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Spend(nil), s.list...)
}
