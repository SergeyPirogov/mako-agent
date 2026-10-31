package dispatch

import (
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
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
		if got := wantsPlan(config.Terminal{PlanFirst: c.mode}, c.task, nil); got != c.want {
			t.Errorf("%q %q/%v: %v", c.mode, c.task.Description[:min(10, len(c.task.Description))], c.task.Labels, got)
		}
	}
	// The estimate of the task's current text decides over its length; one of an older text does not.
	big := &core.Estimate{State: "done", Size: "big", For: estimateKey(small)}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, small, big) {
		t.Error("a big estimate wants a plan")
	}
	if wantsPlan(config.Terminal{PlanFirst: "big"}, long, &core.Estimate{State: "done", Size: "small", For: estimateKey(long)}) {
		t.Error("a small estimate of a long description wants no plan")
	}
	if wantsPlan(config.Terminal{PlanFirst: "big"}, core.Task{Description: "інший текст"}, big) {
		t.Error("an estimate of another text decides nothing")
	}
	if !wantsPlan(config.Terminal{PlanFirst: "big"}, labelled, &core.Estimate{State: "done", Size: "small", For: estimateKey(labelled)}) {
		t.Error("the label wins over the estimate")
	}
	if wantsPlan(config.Terminal{PlanFirst: "never"}, small, big) {
		t.Error("never is never")
	}
	if workMode(config.Terminal{PermissionMode: "acceptEdits"}) != "acceptEdits" || workMode(config.Terminal{PermissionMode: "plan"}) != "auto" {
		t.Error("work mode")
	}
}
