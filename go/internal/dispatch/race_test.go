package dispatch

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// The board, CheckGates and the MR's description read records while the checks change them: what Store.Get hands
// out must share nothing with the stored record (go test -race).
func TestStoreReadersBesideMaybeCheck(t *testing.T) {
	d, _, _, id, wt, _, idle := checksSetup(t, nil, "echo vet fine", "sleep 0.2")
	commit(t, wt, "pay.go")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	reader := func(read func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					read()
				}
			}
		}()
	}
	// What the board's JSON, CheckGates and mrDescription do: copy a record and walk what it points to.
	reader(func() {
		r, _ := d.Store.Get(id)
		if _, err := json.Marshal(r); err != nil {
			t.Error(err)
		}
		if r.Checks != nil {
			_ = r.Checks.Error + r.Checks.State
			_ = len(r.Checks.Results)
		}
	})
	reader(func() {
		for _, r := range d.Store.All() {
			if _, err := json.Marshal(r); err != nil {
				t.Error(err)
			}
		}
	})
	// Changes to the pointers and slices the checks and the answers make in place.
	reader(func() {
		d.Store.Update(id, func(r *core.Record) {
			if r.Checks != nil {
				r.Checks.Error = "push failed"
			}
			r.Task.Comments = append(r.Task.Comments, core.Comment{Author: "me", Body: "an answer"})
		})
	})

	checkNow(t, d, id)
	close(stop)
	wg.Wait()
	idle()
	if r, _ := d.Store.Get(id); r.Checks == nil || len(r.Checks.Results) != 2 {
		t.Fatalf("checks %+v", r.Checks)
	}
}

// ensureProject adds projects to the tracker's map while configuredRepos reads it (go test -race): both under d.mu.
func TestConfiguredReposBesideEnsureProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := setup(t)
	tr := d.TrackerList()[0]
	tr.Config().AllProjects = true

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			// The same project from every poll, and a new one each: nothing is added twice.
			d.ensureProject(tr, "grp/shared")
			d.ensureProject(tr, fmt.Sprintf("grp/app%d", i))
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				d.configuredRepos()
			}
		}()
	}
	wg.Wait()
	// grp/app from the settings, grp/shared once, and the eight others.
	if n := len(tr.Config().Projects); n != 10 {
		t.Fatalf("%d projects, want 10", n)
	}
}
