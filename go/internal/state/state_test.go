package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// state.json holds the task history and repo paths: only the owner may read it.
func TestStateFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Update("task-1", func(r *core.Record) {})
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state.json mode = %o, want 600", perm)
	}
}

// A background job that ends after the task was deleted must not bring back an empty record.
func TestUpdateIfExists(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.UpdateIfExists("gone", func(r *core.Record) { r.Summary = "x" }) {
		t.Fatal("updated a record that is not there")
	}
	if _, ok := s.Get("gone"); ok {
		t.Fatal("UpdateIfExists made a record")
	}
	s.Update("here", func(r *core.Record) {})
	if !s.UpdateIfExists("here", func(r *core.Record) { r.Summary = "x" }) {
		t.Fatal("did not update a record that is there")
	}
	if r, _ := s.Get("here"); r.Summary != "x" || r.UpdatedAt == "" {
		t.Fatalf("record %+v", r)
	}
}

// Get, All and Update hand out copies that share nothing with the stored record: Update changing the record
// (a pointer's field, a slice's append) does not reach a copy a reader holds, and the reader changing its copy
// does not reach the store.
func TestCopiesShareNothingWithTheStore(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Update("a", func(r *core.Record) {
		r.Checks = &core.Checks{State: "passed"}
		r.Estimate = &core.Estimate{Questions: "old"}
		r.Task.Comments = []core.Comment{{Author: "me", Body: "one"}}
	})
	held, _ := s.Get("a")
	all := s.All()
	returned := s.Update("a", func(r *core.Record) {
		r.Checks.Error = "boom"
		r.Estimate.Questions = "new"
		r.Task.Comments = append(r.Task.Comments, core.Comment{Body: "two"})
	})
	for name, r := range map[string]core.Record{"Get": held, "All": all[0]} {
		if r.Checks.Error != "" || r.Estimate.Questions != "old" || len(r.Task.Comments) != 1 {
			t.Errorf("%s: the copy changed with the store: %+v %+v", name, r.Checks, r.Estimate)
		}
	}
	// The copy Update returns is the reader's too.
	returned.Checks.Error = "mine"
	returned.Task.Comments[0].Body = "mine"
	held, _ = s.Get("a")
	if held.Checks.Error != "boom" || held.Task.Comments[0].Body != "one" {
		t.Errorf("changing the returned copy reached the store: %+v %+v", held.Checks, held.Task.Comments)
	}
}

// Readers of copies run beside Updates (go test -race): what Get hands out is never the stored memory.
func TestGetBesideUpdateDoesNotRace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Update("a", func(r *core.Record) { r.Checks = &core.Checks{} })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.Update("a", func(r *core.Record) {
				r.Checks.Error = "e"
				r.Task.Comments = append(r.Task.Comments, core.Comment{Body: "c"})
			})
		}
	}()
	for i := 0; i < 200; i++ {
		r, _ := s.Get("a")
		_ = r.Checks.Error
		_ = len(r.Task.Comments)
		for _, x := range s.All() {
			_ = x.Checks.Error
		}
	}
	<-done
}

// BenchmarkAll: the board's poll over a realistic number of records, each with the metrics of a finished task.
func BenchmarkAll(b *testing.B) {
	s := benchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.All()
	}
}

// BenchmarkUpdate: what every Update costs under the same lock (it writes all the records to state.json).
func BenchmarkUpdate(b *testing.B) {
	s := benchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Update("grp/app#1", func(r *core.Record) { r.Attempt++ })
	}
}

func benchStore(b *testing.B) *Store {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		s.Update(fmt.Sprintf("grp/app#%d", i), func(r *core.Record) {
			run := core.ContextRun{Phase: "work", Tools: map[string]int{"Read": 1, "Bash": 2, "Edit": 3}, Steps: make([]int, 60), Compacted: make([]int, 3),
				Files: make([]core.Heavy, 10), Commands: make([]core.Heavy, 10)}
			r.Metrics = &core.Metrics{Seconds: map[string]float64{"work": 1}, Cost: map[string]float64{"work": 1}, Tokens: map[string]int{"work": 1},
				Context: []core.ContextRun{run, run, run, run}}
			r.Task.Comments = make([]core.Comment, 10)
			r.AgentReviews = make([]core.AgentReview, 2)
			r.Checks = &core.Checks{Results: make([]core.CheckResult, 3)}
		})
	}
	return s
}

// A broken state.json does not keep Mako from starting: it is kept aside as state.json.broken and the board
// starts empty; what is saved after that is read back.
func TestBrokenStateKeptAside(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.All()) != 0 {
		t.Fatalf("records = %v", s.All())
	}
	want := filepath.Join(dir, "state.json.broken")
	if s.Broken != want {
		t.Fatalf("Broken = %q, want %q", s.Broken, want)
	}
	if b, _ := os.ReadFile(want); string(b) != "{broken" {
		t.Fatalf("state.json.broken = %q", b)
	}
	s.Update("task-1", func(r *core.Record) { r.Status = core.Snoozed })
	again, err := Open(dir)
	if err != nil || again.Broken != "" {
		t.Fatalf("%v %q", err, again.Broken)
	}
	if _, ok := again.Get("task-1"); !ok {
		t.Fatal("not saved after the start from scratch")
	}
}

// A change that cannot reach the disk (no rights, a full disk) is told to OnSaveError, not swallowed.
func TestStateSaveErrorIsTold(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var told []error
	s.OnSaveError = func(err error) {
		// Outside the lock: the store can be read from here.
		s.All()
		told = append(told, err)
	}
	s.Update("task-1", func(r *core.Record) {})
	if len(told) != 0 {
		t.Fatalf("an error on a good save: %v", told)
	}
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	s.Update("task-2", func(r *core.Record) {})
	s.UpdateIfExists("task-2", func(r *core.Record) {})
	s.Delete("task-1")
	if len(told) != 3 {
		t.Fatalf("errors told = %v, want 3", told)
	}
}
