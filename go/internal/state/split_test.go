package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// A state.json from before the split reads as it did: no proposal, no parent; a split task's proposal and its
// subtasks' parent come back as saved.
func TestSplitFieldsInState(t *testing.T) {
	dir := t.TempDir()
	old := `{"grp/app#5": {"id": "grp/app#5", "status": "snoozed", "task": {"id": "grp/app#5", "title": "Велика"}, "updatedAt": "2026-01-01T00:00:00Z",
	  "estimate": {"at": "2026-01-01T00:00:00Z", "state": "done", "size": "big", "split": true, "clarity": {"value": 3}, "risk": {"value": 2}}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := s.Get("grp/app#5")
	if !ok || r.Breakdown != nil || r.ParentID != "" || r.Column(0) != "new" || !r.Estimate.Split {
		t.Fatalf("old record %+v", r)
	}

	s.Update("grp/app#5", func(r *core.Record) {
		r.Status = core.Split
		r.Breakdown = &core.Breakdown{State: "created", Items: []core.SplitItem{{Title: "A", Task: "grp/app#6"}, {Title: "B", After: []int{1}, Task: "grp/app#7"}}}
	})
	s.Update("grp/app#6", func(r *core.Record) { r.Status, r.ParentID = core.Snoozed, "grp/app#5" })
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = again.Get("grp/app#5")
	if r.Column(0) != "split" || len(r.Breakdown.Items) != 2 || r.Breakdown.Items[1].After[0] != 1 {
		t.Fatalf("split record %+v", r.Breakdown)
	}
	if c, _ := again.Get("grp/app#6"); c.ParentID != "grp/app#5" {
		t.Errorf("subtask's parent %q", c.ParentID)
	}
	again.Update("grp/app#5", func(r *core.Record) { r.Breakdown.ClosedAt = "2026-02-01T00:00:00Z" })
	if r, _ := again.Get("grp/app#5"); r.Column(0) != "done" {
		t.Errorf("a closed split is in %q", r.Column(0))
	}
}
