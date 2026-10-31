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
		if got := wantsPlan(config.Terminal{PlanFirst: c.mode}, c.task); got != c.want {
			t.Errorf("%q %q/%v: %v", c.mode, c.task.Description[:min(10, len(c.task.Description))], c.task.Labels, got)
		}
	}
	if workMode(config.Terminal{PermissionMode: "acceptEdits"}) != "acceptEdits" || workMode(config.Terminal{PermissionMode: "plan"}) != "auto" {
		t.Error("work mode")
	}
}
