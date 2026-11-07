package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// From an idea to tasks (core.Spec): «New idea» on the board takes a few sentences (and pictures) for a project;
// the agent reads the project's code and writes a specification, asks what it cannot tell (answered on the
// board, one round) and proposes 1–6 tasks with the ones each waits for. Nothing reaches the tracker until
// «Create in tracker»: then one task, or a parent issue with the specification and its subtasks, opened as a
// split's (openSubtasks): each waits for the ones before it to be merged (Record.After), and Mako closes the parent
// once they all are (CheckSplits).

// SpecFrom asks the agent for the specification; tests replace it.
var SpecFrom = writer.Spec

// NewSpecID: the id pictures are attached under for an idea that has no id yet.
const NewSpecID = "spec-new"

const (
	specFile = "specs.json"
	// specTextMax: the longest idea, in characters: a few sentences, not a specification.
	specTextMax = 4000
	// specImagesMax: the most pictures an idea takes.
	specImagesMax = 5
	// specBodyMax: the longest specification the parent issue takes, as edited on the board.
	specBodyMax = 30000
	// specMost: the most tasks an idea is split into.
	specMost = 6
	// specStale: a run that started this long ago and never ended, Mako having stopped meanwhile.
	specStale = writer.SpecTimeout + 5*time.Minute
	// specKept: how long an idea whose tasks are created stays on the board.
	specKept = 48 * time.Hour
)

// SpecView is an idea as the board shows it.
type SpecView struct {
	core.Spec
	// Links: the board's links to its pictures; URL: its parent issue (or its one task) once created.
	Links []string `json:"links,omitempty"`
	URL   string   `json:"url,omitempty"`
}

// --- specs.json

func (d *Dispatcher) specsFile() string {
	if d.Config().StateDir == "" {
		return ""
	}
	return filepath.Join(d.Config().StateDir, specFile)
}

// readSpecs: the ideas; none when the file is missing (an older Mako) or unreadable. Under specsMu.
func (d *Dispatcher) readSpecs() []core.Spec {
	var specs []core.Spec
	if d.specsFile() == "" {
		return nil
	}
	if b, err := os.ReadFile(d.specsFile()); err == nil {
		if err := json.Unmarshal(b, &specs); err != nil {
			log.Printf("%s: not read: %v", specFile, err)
			return nil
		}
	}
	return specs
}

func (d *Dispatcher) writeSpecs(specs []core.Spec) error {
	if d.specsFile() == "" {
		return errors.New("no folder for Mako's state")
	}
	b, _ := json.MarshalIndent(specs, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.specsFile()), 0o755); err != nil {
		return err
	}
	return fsx.WriteFile(d.specsFile(), b, 0o600)
}

// spec: the idea with the id, as kept.
func (d *Dispatcher) spec(id string) (core.Spec, bool) {
	d.specsMu.Lock()
	defer d.specsMu.Unlock()
	for _, s := range d.readSpecs() {
		if s.ID == id {
			return s, true
		}
	}
	return core.Spec{}, false
}

// updateSpec changes the idea with the id and keeps it; false when there is no such idea (dropped meanwhile).
func (d *Dispatcher) updateSpec(id string, change func(*core.Spec)) (core.Spec, bool, error) {
	d.specsMu.Lock()
	defer d.specsMu.Unlock()
	specs := d.readSpecs()
	for i := range specs {
		if specs[i].ID == id {
			change(&specs[i])
			return specs[i], true, d.writeSpecs(specs)
		}
	}
	return core.Spec{}, false, nil
}

// specState: the idea's state, a run that never ended (Mako stopped) being a failed one.
func specState(s core.Spec) string {
	if s.State == "running" {
		if t, err := time.Parse(time.RFC3339Nano, s.RunAt); err != nil || time.Since(t) > specStale {
			return "failed"
		}
	}
	return s.State
}

// Specs: the ideas for the board, the newest first; one whose tasks were created a while ago is gone from it.
func (d *Dispatcher) Specs() []SpecView {
	d.specsMu.Lock()
	specs := d.readSpecs()
	d.specsMu.Unlock()
	out := []SpecView{}
	for _, s := range specs {
		if s.State == "created" {
			if t, err := time.Parse(time.RFC3339Nano, s.Created); err == nil && time.Since(t) > specKept {
				continue
			}
		}
		if st := specState(s); st != s.State {
			s.State, s.Error = st, "Mako stopped while the agent was writing: try again"
		}
		v := SpecView{Spec: s}
		for _, p := range s.Images {
			v.Links = append(v.Links, "/api/screenshots/"+safeFile(s.ID)+"/"+filepath.Base(p))
		}
		created := s.Parent
		if created == "" && len(s.Items) == 1 {
			created = s.Items[0].Task
		}
		if r, ok := d.Store.Get(created); ok && created != "" {
			v.URL = r.Task.URL
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// WaitSpecs waits for the agent's runs going on (tests).
func (d *Dispatcher) WaitSpecs() { d.specRuns.Wait() }

// --- the agent writes

// NewSpec: «Write spec»: keeps the idea for the project and has the agent write its specification in the background.
// images: pictures attached under NewSpecID; they move to the idea's own folder.
func (d *Dispatcher) NewSpec(trackerID, project, text string, images []string) (string, error) {
	text, project = strings.TrimSpace(text), strings.TrimSpace(project)
	switch {
	case text == "":
		return "", errors.New("write the idea")
	case utf8.RuneCountInString(text) > specTextMax:
		return "", fmt.Errorf("the idea is longer than %d characters: a few sentences are enough", specTextMax)
	case project == "":
		return "", errors.New("choose a project")
	case len(images) > specImagesMax:
		return "", fmt.Errorf("at most %d pictures", specImagesMax)
	}
	if _, _, err := d.creator(trackerID); err != nil {
		return "", err
	}
	from := d.ScreenshotDir(NewSpecID)
	for _, p := range images {
		if filepath.Dir(filepath.Clean(p)) != from || !exists(p) {
			return "", errors.New("attach the picture again")
		}
	}
	id := fmt.Sprintf("spec-%d", time.Now().UnixNano())
	var kept []string
	if len(images) > 0 {
		dir := d.ScreenshotDir(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		for _, p := range images {
			to := filepath.Join(dir, filepath.Base(p))
			if err := os.Rename(p, to); err != nil {
				return "", err
			}
			kept = append(kept, to)
		}
	}
	s := core.Spec{ID: id, Tracker: trackerID, Project: project, Text: text, Images: kept, At: now(), State: "running", RunAt: now()}
	d.specsMu.Lock()
	err := d.writeSpecs(append(d.readSpecs(), s))
	d.specsMu.Unlock()
	if err != nil {
		return "", err
	}
	log.Printf("[%s] a new idea for %s: the agent writes its specification", id, project)
	d.sweepSpecPics()
	d.launchSpec(id)
	return id, nil
}

// launchSpec runs the agent on the idea in the background; DropSpec stops it (specStops).
func (d *Dispatcher) launchSpec(id string) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &specStop{cancel}
	d.mu.Lock()
	if was := d.specStops[id]; was != nil {
		was.cancel()
	}
	d.specStops[id] = run
	d.mu.Unlock()
	d.specRuns.Add(1)
	go func() {
		defer d.specRuns.Done()
		defer func() {
			cancel()
			d.mu.Lock()
			// A run started after this one (an answer sent meanwhile) keeps its own stop.
			if d.specStops[id] == run {
				delete(d.specStops, id)
			}
			d.mu.Unlock()
		}()
		d.runSpec(ctx, id)
	}()
}

// stopSpec stops the agent writing the idea's specification, if it is.
func (d *Dispatcher) stopSpec(id string) {
	d.mu.Lock()
	run := d.specStops[id]
	delete(d.specStops, id)
	d.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// specStop stops one run of the agent on an idea.
type specStop struct{ cancel context.CancelFunc }

// specPicsKept: how long a picture attached to the form of a new idea («Write spec» not pressed) is kept.
const specPicsKept = 24 * time.Hour

// sweepSpecPics deletes the pictures attached to the form of a new idea that never became one: older than
// specPicsKept (a form still open keeps its own). At Mako's start and with each new idea.
func (d *Dispatcher) sweepSpecPics() {
	if d.Config().StateDir == "" {
		return
	}
	dir := d.ScreenshotDir(NewSpecID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > specPicsKept {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// runSpec: the agent, in the project's clone fresh from origin, writes the idea's specification: its questions, or
// (they answered) its tasks.
func (d *Dispatcher) runSpec(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, writer.SpecTimeout+20*time.Minute) // a clone first, maybe
	defer cancel()
	s, ok := d.spec(id)
	if !ok {
		return
	}
	draft, err := d.writeSpec(ctx, s)
	if err != nil {
		log.Printf("[%s] the agent did not write the specification: %v", id, err)
	}
	_, _, saveErr := d.updateSpec(id, func(s *core.Spec) {
		if s.State != "running" {
			return
		}
		if err != nil {
			s.State, s.Error = "failed", err.Error()
			return
		}
		s.Title, s.Body, s.Error = draft.Title, draft.Body, ""
		if len(draft.Questions) > 0 && !s.Answered {
			s.State, s.Questions, s.Items = "questions", draft.Questions, nil
			return
		}
		s.State, s.Items = "ready", draft.Items
	})
	if saveErr != nil {
		d.Fault("mako", specFile+" not saved", saveErr)
	}
}

func (d *Dispatcher) writeSpec(ctx context.Context, s core.Spec) (core.SpecDraft, error) {
	t := d.tracker(s.Tracker)
	if t == nil {
		return core.SpecDraft{}, fmt.Errorf("no tracker %q", s.Tracker)
	}
	task := core.Task{TrackerID: s.Tracker, Project: s.Project}
	repo, err := d.route(t, task)
	if err != nil {
		return core.SpecDraft{}, err
	}
	logf := func(line string) { log.Printf("[%s] %s", s.ID, line) }
	if err := gitx.EnsureClone(ctx, repo, logf); err != nil {
		return core.SpecDraft{}, err
	}
	fetchBase(ctx, repo, logf)
	spend := writer.WithSpend(ctx, func(sp writer.Spend) {
		d.updateSpec(s.ID, func(s *core.Spec) { s.Cost += sp.USD })
	})
	in := writer.SpecInput{Text: s.Text, Images: s.Images}
	if s.Answered {
		in.Questions, in.Final = s.Questions, true
	}
	// Writing a specification is planning: the plan's route.
	return SpecFrom(spend, repo.Path, repo.BaseBranch, d.routeOf(core.Record{Task: task}, core.PhasePlan), in)
}

// AnswerSpec: the answers to the agent's questions (by the question's text; none: its assumption stands); the
// agent writes the specification again with them, and its tasks.
func (d *Dispatcher) AnswerSpec(id string, answers []Answer) error {
	byQ := map[string]string{}
	for _, a := range answers {
		byQ[strings.TrimSpace(a.Q)] = strings.TrimSpace(a.A)
	}
	var why error
	_, ok, err := d.updateSpec(id, func(s *core.Spec) {
		if specState(*s) != "questions" {
			why = errors.New("the idea has no questions to answer")
			return
		}
		for i := range s.Questions {
			s.Questions[i].A = clipTo(byQ[s.Questions[i].Q], 2000)
		}
		s.Answered, s.State, s.RunAt, s.Error = true, "running", now(), ""
	})
	if !ok {
		return errors.New("no such idea")
	}
	if why != nil {
		return why
	}
	if err != nil {
		return err
	}
	d.launchSpec(id)
	return nil
}

// RetrySpec: «Try again» after the agent failed.
func (d *Dispatcher) RetrySpec(id string) error {
	var why error
	_, ok, err := d.updateSpec(id, func(s *core.Spec) {
		if specState(*s) != "failed" {
			why = errors.New("the agent is not stopped on this idea")
			return
		}
		s.State, s.RunAt, s.Error = "running", now(), ""
	})
	if !ok {
		return errors.New("no such idea")
	}
	if why != nil {
		return why
	}
	if err != nil {
		return err
	}
	d.launchSpec(id)
	return nil
}

// DropSpec: «Drop»: the idea and its pictures go. One whose tasks are being opened, or are open in part, stays:
// «Create» opens the rest.
func (d *Dispatcher) DropSpec(id string) error {
	d.mu.Lock()
	creating := d.specCreating[id]
	d.mu.Unlock()
	if creating {
		return errors.New("the tasks are being created")
	}
	d.specsMu.Lock()
	specs := d.readSpecs()
	i := slices.IndexFunc(specs, func(s core.Spec) bool { return s.ID == id })
	if i < 0 {
		d.specsMu.Unlock()
		return errors.New("no such idea")
	}
	if s := specs[i]; s.State != "created" && (s.Parent != "" || createdAny(s.Items)) {
		d.specsMu.Unlock()
		return errors.New("some tasks are already in the tracker: create the rest")
	}
	err := d.writeSpecs(slices.Delete(specs, i, i+1))
	d.specsMu.Unlock()
	if err != nil {
		return err
	}
	// The agent still writing for it spends on an idea that is gone.
	d.stopSpec(id)
	os.RemoveAll(d.ScreenshotDir(id))
	log.Printf("[%s] the idea was dropped", id)
	return nil
}

// --- the tasks in the tracker

// CreateSpec: «Create in tracker»: the specification and tasks as edited on the board go to the tracker. One task
// is opened as it is, with the specification in its description; several, as the subtasks of a parent issue that
// holds the specification (a split: Mako closes it once they all are merged). A run that failed half way opens
// the rest.
func (d *Dispatcher) CreateSpec(ctx context.Context, id, title, body string, items []core.SplitItem) error {
	d.mu.Lock()
	if d.specCreating[id] {
		d.mu.Unlock()
		return errors.New("the tasks are being created")
	}
	d.specCreating[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.specCreating, id)
		d.mu.Unlock()
	}()

	s, ok := d.spec(id)
	if !ok {
		return errors.New("no such idea")
	}
	if st := specState(s); st != "ready" && st != "creating" {
		return errors.New("the idea has no tasks to create yet")
	}
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	switch {
	case title == "":
		return errors.New("enter a title")
	case body == "":
		return errors.New("the specification is empty")
	case utf8.RuneCountInString(body) > specBodyMax:
		return fmt.Errorf("the specification is longer than %d characters", specBodyMax)
	case len(items) < 1 || len(items) > specMost:
		return fmt.Errorf("an idea makes 1 to %d tasks, not %d", specMost, len(items))
	case s.Parent != "" && len(items) == 1:
		return errors.New("the parent issue is already in the tracker: keep two or more tasks")
	}
	t, c, err := d.creator(s.Tracker)
	if err != nil {
		return err
	}
	items, err = checkItems(items, s.Items)
	if err != nil {
		return err
	}
	save := func(state, why string) {
		if _, _, err := d.updateSpec(id, func(s *core.Spec) {
			s.Title, s.Body, s.Items, s.State, s.Error = title, body, slices.Clone(items), state, why
		}); err != nil {
			d.Fault("mako", specFile+" not saved", err)
		}
	}
	fail := func(err error) error {
		save("ready", err.Error())
		log.Printf("[%s] the tasks were not created: %v", id, err)
		return err
	}
	save("creating", "")

	// The pictures go up once, for the description.
	if len(s.Pictures) < len(s.Images) {
		var pics []string
		for _, p := range s.Images {
			data, err := os.ReadFile(p)
			if err != nil {
				return fail(fmt.Errorf("picture %s: %v", filepath.Base(p), err))
			}
			md, err := c.Upload(ctx, s.Project, filepath.Base(p), data)
			if err != nil {
				return fail(fmt.Errorf("picture %s was not uploaded: %v", filepath.Base(p), err))
			}
			pics = append(pics, md)
		}
		s.Pictures = pics
		d.updateSpec(id, func(s *core.Spec) { s.Pictures = pics })
	}
	full := strings.TrimSpace(body + "\n\n" + strings.Join(s.Pictures, "\n"))

	if len(items) == 1 {
		if items[0].Task == "" {
			n := trackers.NewIssue{Project: s.Project, Title: items[0].Title, Description: strings.TrimSpace(full + "\n\n---\n\n" + items[0].Description)}
			task, err := d.createTask(ctx, t.ID(), n, false, false, "", 0, "", nil)
			if err != nil {
				return fail(err)
			}
			items[0].Task = task
		}
	} else {
		parent, err := d.specParent(ctx, t, c, id, s, title, full, items)
		if err != nil {
			return fail(err)
		}
		if err := d.openSubtasks(ctx, t, parent, items, func() {
			save("creating", "")
			d.Store.UpdateIfExists(parent.ID, func(r *core.Record) {
				if r.Breakdown != nil {
					b := *r.Breakdown
					b.Items = slices.Clone(items)
					r.Breakdown = &b
				}
			})
		}); err != nil {
			return fail(err)
		}
		d.splitDone(ctx, t, parent, items)
	}
	if _, _, err := d.updateSpec(id, func(s *core.Spec) {
		s.Title, s.Body, s.Items, s.State, s.Error, s.Created = title, body, slices.Clone(items), "created", "", now()
	}); err != nil {
		d.Fault("mako", specFile+" not saved", err)
	}
	log.Printf("[%s] the idea is in the tracker: %d tasks", id, len(items))
	return nil
}

// specParent: the idea's parent issue, opened now unless it was before. On the board it is split at once (its
// subtasks to come), so it is neither estimated nor taken: the work is in its subtasks.
func (d *Dispatcher) specParent(ctx context.Context, t trackers.Tracker, c trackers.Creator, id string, s core.Spec, title, full string, items []core.SplitItem) (core.Task, error) {
	if s.Parent != "" {
		r, ok := d.Store.Get(s.Parent)
		if !ok {
			return core.Task{}, fmt.Errorf("the parent issue %s is not on the board any more", s.Parent)
		}
		return r.Task, nil
	}
	task, err := c.Create(ctx, trackers.NewIssue{Project: s.Project, Title: title, Description: full})
	if err != nil {
		return core.Task{}, err
	}
	d.ensureProject(t, task.Project)
	d.Store.Update(task.ID, func(r *core.Record) {
		r.Task, r.Status = task, core.Split
		r.Breakdown = &core.Breakdown{At: now(), State: "creating", For: EstimateKey(task), Items: slices.Clone(items)}
	})
	d.Log(task.ID, "created from an idea on the board: "+task.Title)
	if _, _, err := d.updateSpec(id, func(s *core.Spec) { s.Parent = task.ID }); err != nil {
		d.Fault("mako", specFile+" not saved", err)
	}
	return task, nil
}
