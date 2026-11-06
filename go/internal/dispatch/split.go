package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The split of a big task (core.Breakdown): «Розбити» on a new task (or its estimate saying split) asks the agent
// for 2–6 subtasks; you edit the list on the board, and «Створити» opens them in the tracker one by one, in the
// task's project, each with «Частина #N» and a «Після #M» for each subtask it waits for. The task itself is then
// split: it waits, out of «Нові», and Mako closes it in the tracker once all its subtasks are merged.

// SplitFrom asks the model for the subtasks; tests replace it.
var SplitFrom = writer.Split

// splitMost, splitLeast: how many subtasks a split has.
const splitLeast, splitMost = 2, 6

// splitStale: a proposal that started this long ago and never ended, Mako having stopped meanwhile.
const splitStale = writer.SplitTimeout + 5*time.Minute

// SplitTask: «Розбити»: the agent proposes the subtasks of a new task, in the background.
func (d *Dispatcher) SplitTask(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !isNew(r) {
		return errors.New("only a new task can be split")
	}
	if _, _, err := d.creator(r.Task.TrackerID); err != nil {
		return err
	}
	if b := r.Breakdown; b != nil && b.State == "running" && !splitIsStale(b.At) {
		return errors.New("the agent is already splitting the task")
	}
	if b := r.Breakdown; b != nil && createdAny(b.Items) {
		return errors.New("some subtasks are already in the tracker: create the rest")
	}
	d.Store.UpdateIfExists(id, func(r *core.Record) {
		r.Breakdown = &core.Breakdown{At: now(), State: "running", For: EstimateKey(r.Task)}
	})
	d.splits.Add(1)
	go func() {
		defer d.splits.Done()
		d.split(context.Background(), id)
	}()
	return nil
}

// WaitSplits waits for the proposals running (tests).
func (d *Dispatcher) WaitSplits() { d.splits.Wait() }

func splitIsStale(at string) bool {
	t, err := time.Parse(time.RFC3339Nano, at)
	return err != nil || time.Since(t) > splitStale
}

// maybeSplit: the estimate says the task is several in one: the agent proposes its split at once, unless there is
// a proposal already or the task is itself a subtask.
func (d *Dispatcher) maybeSplit(id string) {
	r, ok := d.Store.Get(id)
	// A subtask the estimate finds big again is yours to split: not on its own, in a chain.
	if !ok || r.Estimate == nil || !r.Estimate.Split || r.Breakdown != nil || r.ParentID != "" {
		return
	}
	if err := d.SplitTask(id); err != nil {
		d.Log(id, "the task was not split: "+err.Error())
	}
}

// split asks the agent for the subtasks and keeps them as the list to edit.
func (d *Dispatcher) split(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, writer.SplitTimeout+time.Minute)
	defer cancel()
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	dir := os.TempDir()
	if t := d.tracker(r.Task.TrackerID); t != nil {
		if repo, err := d.route(t, r.Task); err == nil && exists(repo.Path) {
			dir = repo.Path
		}
	}
	// Splitting is planning: the plan's route, not the estimate's quick look; its cost counts before the start.
	items, err := SplitFrom(d.spending(ctx, id, core.PhaseEstimate), dir, d.routeOf(r, core.PhasePlan), r.Task, writer.RepoOutline(ctx, dir))
	if err != nil {
		d.Log(id, "the agent did not split the task: "+err.Error())
	} else {
		d.Log(id, fmt.Sprintf("the agent proposed %d subtasks", len(items)))
	}
	d.Store.UpdateIfExists(id, func(r *core.Record) {
		if r.Breakdown == nil || r.Breakdown.State != "running" {
			return // dropped meanwhile
		}
		b := *r.Breakdown
		if err != nil {
			b.State, b.Error = "failed", err.Error()
		} else {
			b.State, b.Items, b.Error = "done", items, ""
		}
		r.Breakdown = &b
	})
}

// DropSplit: «Не розбивати»: the proposal goes; one already created in the tracker stays.
func (d *Dispatcher) DropSplit(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if b := r.Breakdown; b != nil && (b.State == "created" || createdAny(b.Items)) {
		return errors.New("the subtasks are already in the tracker")
	}
	d.Store.UpdateIfExists(id, func(r *core.Record) { r.Breakdown = nil })
	d.Log(id, "the split was dropped")
	return nil
}

func createdAny(items []core.SplitItem) bool {
	for _, it := range items {
		if it.Task != "" {
			return true
		}
	}
	return false
}

// checkSplit: the list as it can be created: 2–6 subtasks, each with a title, each waiting only for those above
// it. A subtask already created is the one created, as it was.
func checkSplit(items []core.SplitItem, had []core.SplitItem) ([]core.SplitItem, error) {
	if len(items) < splitLeast || len(items) > splitMost {
		return nil, fmt.Errorf("a task splits into %d to %d subtasks, not %d", splitLeast, splitMost, len(items))
	}
	created := map[string]core.SplitItem{}
	for _, it := range had {
		if it.Task != "" {
			created[it.Task] = it
		}
	}
	out := make([]core.SplitItem, len(items))
	for i, it := range items {
		if it.Task != "" {
			was, ok := created[it.Task]
			if !ok {
				return nil, fmt.Errorf("subtask %d is not one created from this task", i+1)
			}
			out[i] = was
			continue
		}
		it.Title, it.Description = strings.TrimSpace(it.Title), strings.TrimSpace(it.Description)
		if it.Title == "" {
			return nil, fmt.Errorf("subtask %d has no title", i+1)
		}
		for _, a := range it.After {
			if a < 1 || a > i {
				return nil, fmt.Errorf("subtask %d can only come after those above it, not after %d", i+1, a)
			}
		}
		out[i] = it
	}
	return out, nil
}

// shortRef: "#12" of "group/app#12", as a tracker links it within the project.
func shortRef(id string) string {
	if i := strings.LastIndex(id, "#"); i >= 0 {
		return id[i:]
	}
	return id
}

// CreateSplit: «Створити»: opens the subtasks of the list, as edited on the board, in the tracker, in order; the
// ones created before (a run that failed half way) are not opened again. Then the task is split: a comment in the
// tracker lists its subtasks, and it leaves «Нові».
func (d *Dispatcher) CreateSplit(ctx context.Context, id string, items []core.SplitItem) error {
	d.mu.Lock()
	if d.splitting[id] {
		d.mu.Unlock()
		return errors.New("the subtasks are being created")
	}
	d.splitting[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.splitting, id)
		d.mu.Unlock()
	}()

	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !isNew(r) {
		return errors.New("only a new task can be split")
	}
	t, _, err := d.creator(r.Task.TrackerID)
	if err != nil {
		return err
	}
	var had []core.SplitItem
	if r.Breakdown != nil {
		had = r.Breakdown.Items
	}
	items, err = checkSplit(items, had)
	if err != nil {
		return err
	}
	save := func(state, why string) {
		d.Store.UpdateIfExists(id, func(r *core.Record) {
			b := core.Breakdown{At: now(), For: EstimateKey(r.Task)}
			if r.Breakdown != nil {
				b = *r.Breakdown
			}
			// A copy: items changes below, outside the store's lock.
			b.State, b.Items, b.Error = state, slices.Clone(items), why
			r.Breakdown = &b
		})
	}
	save("done", "")
	parent := shortRef(r.Task.ID)
	for i := range items {
		if items[i].Task != "" {
			continue
		}
		lines := []string{i18n.TA("split.partOf", parent)}
		for _, a := range items[i].After {
			lines = append(lines, i18n.TA("split.after", shortRef(items[a-1].Task)))
		}
		desc := strings.TrimSpace(items[i].Description + "\n\n" + strings.Join(lines, "\n"))
		n := trackers.NewIssue{Project: r.Task.Project, Title: items[i].Title, Description: desc}
		child, err := d.createTask(ctx, t.ID(), n, false, false, "", 0, id)
		if err != nil {
			why := fmt.Sprintf("created %d of %d subtasks: %v", countCreated(items), len(items), err)
			save("done", why)
			d.Log(id, "the split stopped: "+why)
			return errors.New(why)
		}
		items[i].Task = child
		save("done", "")
	}
	list := []string{i18n.TA("split.comment")}
	for _, it := range items {
		list = append(list, "- "+shortRef(it.Task)+" "+it.Title)
	}
	if err := t.Comment(ctx, r.Task, strings.Join(list, "\n")); err != nil {
		d.Log(id, "the list of subtasks was not commented in the tracker: "+err.Error())
	}
	d.Store.UpdateIfExists(id, func(r *core.Record) {
		b := *r.Breakdown
		b.State, b.Error = "created", ""
		r.Breakdown, r.Status, r.AutoSkip = &b, core.Split, ""
	})
	d.Log(id, fmt.Sprintf("split into %d subtasks", len(items)))
	return nil
}

func countCreated(items []core.SplitItem) int {
	n := 0
	for _, it := range items {
		if it.Task != "" {
			n++
		}
	}
	return n
}

// CheckSplits closes each split task in the tracker once all its subtasks are finished: merged, or closed or
// deleted in the tracker (you decided it is not needed). A subtask that left the board keeps the task open.
func (d *Dispatcher) CheckSplits(ctx context.Context) {
	for _, r := range d.Store.All() {
		if r.Status != core.Split || r.Breakdown == nil || r.Breakdown.ClosedAt != "" || !splitFinished(d, r.Breakdown) {
			continue
		}
		t := d.tracker(r.Task.TrackerID)
		if t == nil {
			continue
		}
		status, err := t.Move(ctx, r.Task, trackers.AtMerged)
		if err != nil {
			// Tried again next round.
			d.Store.UpdateIfExists(r.ID, func(r *core.Record) { r.TrackerStatus = &core.TrackerState{OK: false, Text: err.Error()} })
			d.Log(r.ID, "all subtasks are done, but the task was not closed in the tracker: "+err.Error())
			continue
		}
		at := now()
		d.Store.UpdateIfExists(r.ID, func(r *core.Record) {
			b := *r.Breakdown
			b.ClosedAt = at
			r.Breakdown, r.FinishedAt = &b, at
			if status != "" {
				r.TrackerStatus = &core.TrackerState{OK: true, Text: status}
			}
		})
		d.Log(r.ID, "all subtasks are done: the task is closed in the tracker")
	}
}

func splitFinished(d *Dispatcher, b *core.Breakdown) bool {
	if len(b.Items) == 0 {
		return false
	}
	for _, it := range b.Items {
		c, ok := d.Store.Get(it.Task)
		if it.Task == "" || !ok || c.ReviewState != "merged" && c.Gone == "" {
			return false
		}
	}
	return true
}
