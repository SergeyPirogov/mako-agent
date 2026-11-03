package state

import (
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
