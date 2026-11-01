package dispatch

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// --- tasks written on the board: «+ Задача»

// NewTaskProjects: where a task can be written, per tracker that can open issues: the configured projects
// and the ones its tasks on the board come from.
func (d *Dispatcher) NewTaskProjects() map[string][]string {
	out := map[string][]string{}
	records := d.Store.All()
	for _, t := range d.TrackerList() {
		if _, ok := t.(trackers.Creator); !ok {
			continue
		}
		set := map[string]bool{}
		d.mu.Lock()
		for key := range t.Config().Projects {
			set[key] = true
		}
		d.mu.Unlock()
		for _, r := range records {
			if r.Task.TrackerID == t.ID() && r.Task.Project != "" {
				set[r.Task.Project] = true
			}
		}
		list := []string{}
		for p := range set {
			list = append(list, p)
		}
		sort.Strings(list)
		out[t.ID()] = list
	}
	return out
}

func (d *Dispatcher) creator(trackerID string) (trackers.Tracker, trackers.Creator, error) {
	t := d.tracker(trackerID)
	if t == nil {
		return nil, nil, fmt.Errorf("no tracker %q", trackerID)
	}
	c, ok := t.(trackers.Creator)
	if !ok {
		return nil, nil, fmt.Errorf("%s cannot create tasks from the board", t.Name())
	}
	return t, c, nil
}

// CreateTask opens the issue in the tracker, assigned to me, and puts it on the board at once (taken, with
// take); it returns the task's id.
func (d *Dispatcher) CreateTask(ctx context.Context, trackerID string, n trackers.NewIssue, take bool) (string, error) {
	n.Title, n.Project = strings.TrimSpace(n.Title), strings.TrimSpace(n.Project)
	if n.Title == "" {
		return "", fmt.Errorf("enter a task title")
	}
	if n.Project == "" {
		return "", fmt.Errorf("choose a project")
	}
	t, c, err := d.creator(trackerID)
	if err != nil {
		return "", err
	}
	task, err := c.Create(ctx, n)
	if err != nil {
		return "", err
	}
	d.ensureProject(t, task.Project)
	d.Store.Update(task.ID, func(r *core.Record) { r.Task, r.Status = task, core.Snoozed })
	d.Log(task.ID, "created from the board: "+task.Title)
	if take {
		d.Take(ctx, task.ID)
	}
	return task.ID, nil
}

// UploadImage stores an image pasted into a new task's description; it returns the Markdown for it.
func (d *Dispatcher) UploadImage(ctx context.Context, trackerID, project, name string, data []byte) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", fmt.Errorf("choose a project first")
	}
	_, c, err := d.creator(trackerID)
	if err != nil {
		return "", err
	}
	return c.Upload(ctx, project, name, data)
}
