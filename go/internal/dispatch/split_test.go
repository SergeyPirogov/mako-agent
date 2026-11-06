package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// splitAgent makes the agent's proposal the given subtasks (or err) and counts the times it is asked.
func splitAgent(t *testing.T, items []core.SplitItem, err error) *int {
	t.Helper()
	calls := 0
	old := SplitFrom
	SplitFrom = func(_ context.Context, _ string, _ core.Route, _ core.Task, _ string) ([]core.SplitItem, error) {
		calls++
		return items, err
	}
	t.Cleanup(func() { SplitFrom = old })
	return &calls
}

var threeSubtasks = []core.SplitItem{
	{Title: "Модель", Description: "Типи й міграція"},
	{Title: "API", Description: "Хендлери", After: []int{1}},
	{Title: "Дошка", Description: "Картка", After: []int{1, 2}},
}

// «Розбити»: the agent's subtasks become the list to edit; «Створити» opens them in the tracker in order, each with
// its parent and the subtasks it waits for; the task is split, with a comment listing them.
func TestSplitTask(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	const id = "grp/app#5"
	splitAgent(t, threeSubtasks, nil)

	if err := d.SplitTask(id); err != nil {
		t.Fatal(err)
	}
	d.WaitSplits()
	r, _ := d.Store.Get(id)
	if b := r.Breakdown; b == nil || b.State != "done" || len(b.Items) != 3 || b.For != EstimateKey(r.Task) {
		t.Fatalf("proposal %+v", r.Breakdown)
	}

	// Edited on the board: the third gets another title and waits only for the second.
	edited := []core.SplitItem{threeSubtasks[0], threeSubtasks[1], {Title: "Картка на дошці", Description: "Картка", After: []int{2}}}
	if err := d.CreateSplit(ctx, id, edited); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Store.Get(id)
	if r.Status != core.Split || r.Column(0) != "split" || r.Breakdown.State != "created" {
		t.Fatalf("parent %s %s %+v", r.Status, r.Column(0), r.Breakdown)
	}
	want := []struct{ id, title, desc string }{
		{"grp/app#101", "Модель", "Типи й міграція\n\nЧастина #5"},
		{"grp/app#102", "API", "Хендлери\n\nЧастина #5\nПісля #101"},
		{"grp/app#103", "Картка на дошці", "Картка\n\nЧастина #5\nПісля #102"},
	}
	for i, w := range want {
		if got := r.Breakdown.Items[i].Task; got != w.id {
			t.Fatalf("subtask %d is %q, want %q", i+1, got, w.id)
		}
		c, ok := d.Store.Get(w.id)
		if !ok || c.Status != core.Snoozed || c.ParentID != id || c.Task.Title != w.title || c.Task.Description != w.desc {
			t.Errorf("subtask %s: %s %q %q parent %q", w.id, c.Status, c.Task.Title, c.Task.Description, c.ParentID)
		}
	}
	if len(f.notesOf[5]) != 1 || !strings.Contains(f.notesOf[5][0], "- #101 Модель\n- #102 API\n- #103 Картка на дошці") {
		t.Errorf("parent's comment %q", f.notesOf[5])
	}
	// Split: not «Нові», not taken, and the list is no longer for editing.
	if CanTake(r) {
		t.Error("a split task can be taken")
	}
	if err := d.CreateSplit(ctx, id, edited); err == nil {
		t.Error("a split task was split again")
	}
	if err := d.DropSplit(id); err == nil {
		t.Error("a created split was dropped")
	}
	d.Poll(ctx)
	if r, _ := d.Store.Get(id); r.Status != core.Split {
		t.Errorf("the next search made it %s", r.Status)
	}
}

// What «Створити» refuses: too few or too many subtasks, one without a title, one waiting for a subtask below it,
// a task that is not new, a tracker that cannot open issues.
func TestCreateSplitChecks(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	const id = "grp/app#5"
	cases := map[string][]core.SplitItem{
		"one":        {{Title: "Все"}},
		"seven":      {{Title: "1"}, {Title: "2"}, {Title: "3"}, {Title: "4"}, {Title: "5"}, {Title: "6"}, {Title: "7"}},
		"no title":   {{Title: "Модель"}, {Title: "  "}},
		"waits down": {{Title: "Модель", After: []int{2}}, {Title: "API"}},
		"waits self": {{Title: "Модель"}, {Title: "API", After: []int{2}}},
		"not ours":   {{Title: "Модель", Task: "grp/app#77"}, {Title: "API"}},
	}
	for name, items := range cases {
		if err := d.CreateSplit(ctx, id, items); err == nil {
			t.Errorf("%s: created", name)
		}
	}
	if len(f.issues) != 1 {
		t.Fatalf("issues opened by a refused split: %d", len(f.issues))
	}
	d.Store.Update(id, func(r *core.Record) { r.Status = core.Queued })
	if err := d.CreateSplit(ctx, id, threeSubtasks); err == nil {
		t.Error("a task in work was split")
	}
	if err := d.SplitTask(id); err == nil {
		t.Error("a task in work was given to the agent to split")
	}
	if err := d.SplitTask("grp/app#404"); err == nil {
		t.Error("no such task, yet split")
	}
}

// The tracker refuses the second subtask: the first stays created, and «Створити» again opens only the rest.
func TestCreateSplitHalfWay(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	const id = "grp/app#5"
	f.mu.Lock()
	f.createLimited, f.createsLeft = true, 1
	f.mu.Unlock()
	err := d.CreateSplit(ctx, id, threeSubtasks)
	if err == nil || !strings.Contains(err.Error(), "created 1 of 3") {
		t.Fatalf("error %v", err)
	}
	r, _ := d.Store.Get(id)
	if !isNew(r) || r.Breakdown.State != "done" || r.Breakdown.Items[0].Task != "grp/app#101" || r.Breakdown.Items[1].Task != "" {
		t.Fatalf("after the failure: %s %+v", r.Status, r.Breakdown)
	}
	if err := d.DropSplit(id); err == nil {
		t.Error("a split with a created subtask was dropped")
	}
	if err := d.SplitTask(id); err == nil {
		t.Error("a split with a created subtask was proposed anew")
	}

	f.mu.Lock()
	f.createLimited = false
	f.mu.Unlock()
	// The board sends the list back with the created subtask as it has it.
	if err := d.CreateSplit(ctx, id, r.Breakdown.Items); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 4 {
		t.Fatalf("issues: %d, want the parent and three subtasks", len(f.issues))
	}
	if c, _ := d.Store.Get("grp/app#103"); !strings.Contains(c.Task.Description, "Після #101\nПісля #102") {
		t.Errorf("the third waits for %q", c.Task.Description)
	}
}

// The split task closes in the tracker once every subtask is merged or closed there, not before.
func TestCheckSplitsClosesParent(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	const id = "grp/app#5"
	if err := d.CreateSplit(ctx, id, threeSubtasks); err != nil {
		t.Fatal(err)
	}
	merged := func(id string) {
		d.Store.Update(id, func(r *core.Record) { r.Status, r.ReviewState = core.Done, "merged" })
	}
	merged("grp/app#101")
	merged("grp/app#102")
	d.CheckSplits(ctx)
	if r, _ := d.Store.Get(id); r.Breakdown.ClosedAt != "" || f.issues[5]["state"] != "opened" {
		t.Fatal("closed with a subtask still open")
	}
	// The third was closed in the tracker without an MR: not needed, it counts as done.
	d.Store.Update("grp/app#103", func(r *core.Record) { r.Status, r.Gone = core.Cancelled, "closed" })
	d.CheckSplits(ctx)
	r, _ := d.Store.Get(id)
	if r.Breakdown.ClosedAt == "" || r.Column(0) != "done" || f.issues[5]["state"] != "closed" {
		t.Fatalf("not closed: %+v, issue %v", r.Breakdown, f.issues[5]["state"])
	}
	// Once only.
	statuses := len(f.statusSet)
	d.CheckSplits(ctx)
	if len(f.statusSet) != statuses {
		t.Error("closed again")
	}
}

// A subtask that left the board keeps its parent open: Mako cannot tell it is done.
func TestCheckSplitsMissingSubtask(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	const id = "grp/app#5"
	if err := d.CreateSplit(ctx, id, threeSubtasks[:2]); err != nil {
		t.Fatal(err)
	}
	d.Store.Update("grp/app#101", func(r *core.Record) { r.Status, r.ReviewState = core.Done, "merged" })
	d.Store.Delete("grp/app#102")
	d.CheckSplits(ctx)
	if r, _ := d.Store.Get(id); r.Breakdown.ClosedAt != "" || f.issues[5]["state"] != "opened" {
		t.Fatal("closed with a subtask gone from the board")
	}
}

// The agent fails: the proposal says why, and «Розбити» again asks once more. «Не розбивати» drops it.
func TestSplitFailsAndDrop(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	const id = "grp/app#5"
	calls := splitAgent(t, nil, errors.New("no answer from Claude within 5 min"))
	if err := d.SplitTask(id); err != nil {
		t.Fatal(err)
	}
	d.WaitSplits()
	r, _ := d.Store.Get(id)
	if r.Breakdown == nil || r.Breakdown.State != "failed" || !strings.Contains(r.Breakdown.Error, "no answer") {
		t.Fatalf("failed proposal %+v", r.Breakdown)
	}
	if err := d.SplitTask(id); err != nil {
		t.Fatal(err)
	}
	d.WaitSplits()
	if *calls != 2 {
		t.Errorf("asked %d times", *calls)
	}
	if err := d.DropSplit(id); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get(id); r.Breakdown != nil || r.Status != core.Snoozed {
		t.Errorf("after the drop: %s %+v", r.Status, r.Breakdown)
	}
}

// The estimate says split: the agent proposes the subtasks by itself, once; a subtask it says that of is not split
// on its own.
func TestEstimateSplitProposes(t *testing.T) {
	d, _, _ := setup(t)
	dir := t.TempDir()
	script := "#!/bin/sh\ncat > /dev/null\n" +
		`echo '{"clarity":{"score":4,"why":"ясно"},"questions":[],"size":"big","parts":"web","risk":{"score":2,"why":"UI"},"plan":false,"split":true,"model":"","advice":"розбити"}'` + "\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	old := writer.Claude
	writer.Claude = filepath.Join(dir, "claude")
	t.Cleanup(func() { writer.Claude = old })
	calls := splitAgent(t, threeSubtasks, nil)
	d.Cfg.Estimate = true
	ctx := context.Background()

	d.Poll(ctx)
	d.WaitEstimates()
	d.WaitSplits()
	r, _ := d.Store.Get("grp/app#5")
	if *calls != 1 || r.Breakdown == nil || r.Breakdown.State != "done" {
		t.Fatalf("asked %d times, proposal %+v", *calls, r.Breakdown)
	}

	if err := d.CreateSplit(ctx, "grp/app#5", threeSubtasks); err != nil {
		t.Fatal(err)
	}
	d.WaitEstimates()
	d.WaitSplits()
	if c, _ := d.Store.Get("grp/app#101"); c.Estimate == nil || !c.Estimate.Split || c.Breakdown != nil {
		t.Errorf("subtask: estimate %+v, proposal %+v", c.Estimate, c.Breakdown)
	}
	if *calls != 1 {
		t.Errorf("subtasks were split on their own: %d calls", *calls)
	}
}
