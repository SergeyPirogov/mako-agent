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
	big := &core.Estimate{State: "done", Size: "big", For: estimateKey(small)}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, small, big, false) {
		t.Error("a big estimate wants a plan")
	}
	if wantsPlan(config.Terminal{PlanFirst: "big"}, long, &core.Estimate{State: "done", Size: "small", For: estimateKey(long)}, false) {
		t.Error("a small estimate of a long description wants no plan")
	}
	if wantsPlan(config.Terminal{PlanFirst: "big"}, core.Task{Description: "інший текст"}, big, false) {
		t.Error("an estimate of another text decides nothing")
	}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, labelled, &core.Estimate{State: "done", Size: "small", For: estimateKey(labelled)}, false) {
		t.Error("the label wins over the estimate")
	}
	if wantsPlan(config.Terminal{PlanFirst: "never"}, small, big, false) {
		t.Error("never is never")
	}
	// The estimate advises a plan for a medium task; «план спочатку» picked on the card wins over the settings.
	advised := &core.Estimate{State: "done", Size: "medium", Plan: true, For: estimateKey(small)}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, small, advised, false) {
		t.Error("a plan the estimate advises")
	}
	if !wantsPlan(config.Terminal{PlanFirst: "never"}, small, nil, true) {
		t.Error("a plan picked on the card")
	}
	if workMode(config.Terminal{PermissionMode: "acceptEdits"}) != "acceptEdits" || workMode(config.Terminal{PermissionMode: "plan"}) != "auto" {
		t.Error("work mode")
	}
}

// The model: picked for the task first, then the estimate's when the settings take it, else the configured one.
func TestModelFor(t *testing.T) {
	task := core.Task{ID: "g/a#1", Title: "x", Description: "y"}
	e := &core.Estimate{State: "done", Size: "small", Model: "sonnet", For: estimateKey(task)}
	r := core.Record{Task: task, Estimate: e}
	if m := modelFor(&config.Config{}, r); m != "" {
		t.Errorf("settings off: %q", m)
	}
	if m := modelFor(&config.Config{EstimateModel: true}, r); m != "sonnet" {
		t.Errorf("settings on: %q", m)
	}
	r.Model = "opus"
	if m := modelFor(&config.Config{EstimateModel: true}, r); m != "opus" {
		t.Errorf("picked: %q", m)
	}
	r.Model, r.Task.Description = "", "changed"
	if m := modelFor(&config.Config{EstimateModel: true}, r); m != "" {
		t.Errorf("stale estimate: %q", m)
	}
}

// The start model: as modelFor, then the configured one, then Sonnet.
func TestStartModel(t *testing.T) {
	task := core.Task{ID: "g/a#1", Title: "x", Description: "y"}
	r := core.Record{Task: task}
	if m := StartModel(&config.Config{EstimateModel: true}, r); m != "sonnet" {
		t.Errorf("nothing names one: %q", m)
	}
	cfg := &config.Config{EstimateModel: true, Terminal: config.Terminal{Model: "haiku"}}
	if m := StartModel(cfg, r); m != "haiku" {
		t.Errorf("configured: %q", m)
	}
	r.Estimate = &core.Estimate{State: "done", Size: "big", Model: "opus", For: estimateKey(task)}
	if m := StartModel(cfg, r); m != "opus" {
		t.Errorf("estimate: %q", m)
	}
	r.Model = "sonnet"
	if m := StartModel(cfg, r); m != "sonnet" {
		t.Errorf("picked: %q", m)
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
