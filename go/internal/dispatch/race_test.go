package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// The board, CheckGates and the MR's description read records while the checks change them: what Store.Get hands
// out must share nothing with the stored record. It finds the race only under go test -race (CI does not run it).
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
	// What the board's JSON, CheckGates and the MR's description do with the records they read.
	reader(func() {
		r, _ := d.Store.Get(id)
		if _, err := json.Marshal(r); err != nil {
			t.Error(err)
		}
		_ = mrDescription(r, "what changed")
	})
	reader(func() {
		for _, r := range d.Store.All() {
			if _, err := json.Marshal(r); err != nil {
				t.Error(err)
			}
		}
	})
	reader(d.CheckGates)
	// Not a reader: changes the pointers and slices the checks and the answers change in place.
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
	settle(t, d, id) // the checks CheckGates started
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

// Two projects with the same last part of the path, added at once, get different folders.
func TestEnsureProjectPicksOneFolderPerProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := setup(t)
	tr := d.TrackerList()[0]
	tr.Config().AllProjects = true

	for i := 0; i < 20; i++ {
		a, b := fmt.Sprintf("a%d/frontend", i), fmt.Sprintf("b%d/frontend", i)
		var wg sync.WaitGroup
		for _, project := range []string{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.ensureProject(tr, project)
			}()
		}
		wg.Wait()
		paths := map[string]bool{}
		for _, project := range []string{a, b} {
			p := tr.Config().Projects[project]
			if p == nil {
				t.Fatalf("%s was not added", project)
			}
			for _, r := range p.Repos {
				paths[r.Path] = true
			}
		}
		if len(paths) != 2 {
			t.Fatalf("%s and %s share a folder: %v", a, b, paths)
		}
		// Cleared, so the next pair starts with the folder free again.
		d.mu.Lock()
		delete(tr.Config().Projects, a)
		delete(tr.Config().Projects, b)
		d.mu.Unlock()
	}
}

// ensureProject adds projects while a poll (FetchMine) reads the tracker's projects, which holds no d.mu
// (go test -race; a map write during a range is a fatal error in Go).
func TestFetchMineBesideEnsureProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := setup(t)
	tr := d.TrackerList()[0]
	tr.Config().AllProjects = true

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			d.ensureProject(tr, fmt.Sprintf("grp/app%d", i))
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				tr.FetchMine(context.Background()) // errors do not matter: the projects are read either way
			}
		}()
	}
	wg.Wait()
	if n := len(tr.Config().ProjectKeys()); n != 9 {
		t.Fatalf("%d projects, want 9", n)
	}
}
