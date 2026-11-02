package dispatch

import (
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

func TestWantsPlan(t *testing.T) {
	small := core.Task{Description: "Поправити опечатку"}
	labelled := core.Task{Description: "коротко", Labels: []string{"bug", " Plan "}}
	long := core.Task{Description: strings.Repeat("ї", bigDescription)}
	for _, c := range []struct {
		mode string
		task core.Task
		want bool
	}{
		{"", long, false},
		{"never", labelled, false},
		{"always", small, true},
		{"big", small, false},
		{"big", labelled, true},
		{"big", long, true},
		{"big", core.Task{Description: strings.Repeat("ї", bigDescription-1)}, false},
	} {
		if got := wantsPlan(config.Terminal{PlanFirst: c.mode}, c.task, nil, false); got != c.want {
			t.Errorf("%q %q/%v: %v", c.mode, c.task.Description[:min(10, len(c.task.Description))], c.task.Labels, got)
		}
	}
	// The estimate of the task's current text decides over its length; one of an older text does not.
	big := &core.Estimate{State: "done", Size: "big", For: EstimateKey(small)}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, small, big, false) {
		t.Error("a big estimate wants a plan")
	}
	if wantsPlan(config.Terminal{PlanFirst: "big"}, long, &core.Estimate{State: "done", Size: "small", For: EstimateKey(long)}, false) {
		t.Error("a small estimate of a long description wants no plan")
	}
	if wantsPlan(config.Terminal{PlanFirst: "big"}, core.Task{Description: "інший текст"}, big, false) {
		t.Error("an estimate of another text decides nothing")
	}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, labelled, &core.Estimate{State: "done", Size: "small", For: EstimateKey(labelled)}, false) {
		t.Error("the label wins over the estimate")
	}
	if wantsPlan(config.Terminal{PlanFirst: "never"}, small, big, false) {
		t.Error("never is never")
	}
	// The estimate advises a plan for a medium task; «план спочатку» picked on the card wins over the settings.
	advised := &core.Estimate{State: "done", Size: "medium", Plan: true, For: EstimateKey(small)}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, small, advised, false) {
		t.Error("a plan the estimate advises")
	}
	if !wantsPlan(config.Terminal{PlanFirst: "never"}, small, nil, true) {
		t.Error("a plan picked on the card")
	}
	if WorkMode(config.Terminal{PermissionMode: "acceptEdits"}) != "acceptEdits" || WorkMode(config.Terminal{PermissionMode: "plan"}) != "auto" {
		t.Error("work mode")
	}
}

func TestPick(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := New(&config.Config{StateDir: t.TempDir()}, store, nil)
	store.Update("g/a#2", func(r *core.Record) { r.Status, r.Task = core.Asking, core.Task{ID: "g/a#2"} })
	if err := d.Pick("g/a#2", "sonnet", true); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.Get("g/a#2"); r.Model != "sonnet" || !r.PlanFirst {
		t.Fatalf("picked %+v", r)
	}
	store.Update("g/a#2", func(r *core.Record) { r.Worktree = "/wt" })
	if err := d.Pick("g/a#2", "", false); err == nil {
		t.Fatal("picked after the start")
	}
}
