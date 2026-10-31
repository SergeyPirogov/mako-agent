package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// «+ Задача»: the issue opens in the tracker and is on the board at once; the next search keeps it.
func TestCreateTask(t *testing.T) {
	d, _, jobs := setup(t)
	ctx := context.Background()
	if got := d.NewTaskProjects()[d.TrackerList()[0].ID()]; strings.Join(got, ",") != "grp/app" {
		t.Fatalf("projects %v", got)
	}
	if _, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "  "}, false); err == nil {
		t.Fatal("a task without a title was created")
	}
	id, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Експорт у CSV", Description: "Потрібен"}, false)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := d.Store.Get(id)
	if !ok || r.Status != core.Snoozed || r.Task.Title != "Експорт у CSV" || r.Column(0) != "new" {
		t.Fatalf("record %+v", r)
	}
	if !strings.Contains(strings.Join(r.Task.Labels, ","), "claude-agent") || strings.Join(r.Task.Assignees, ",") != "serg" {
		t.Errorf("labels %v assignees %v", r.Task.Labels, r.Task.Assignees)
	}
	d.Poll(ctx)
	if _, ok := d.Store.Get(id); !ok {
		t.Fatal("the next search dropped the created task")
	}

	id2, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Падає збереження", Bug: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	d.Wait()
	if len(*jobs) != 1 || !strings.HasPrefix((*jobs)[0].Branch, "fix/") {
		t.Fatalf("taken at once: %+v", *jobs)
	}
	if r, _ := d.Store.Get(id2); r.Status != core.HandedOff {
		t.Errorf("status %s", r.Status)
	}
}
