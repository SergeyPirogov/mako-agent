package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Eval: a repository's set of merged tasks the agent does again with other settings (see core.EvalCase). The sets
// and their runs are kept in evals.json in Mako's state folder, by navKey; each case's merged diff next to it in
// evals/<case>.diff, the commits it needs under refs/mako/eval/<case> in the clone.

// EvalSet is a repository's eval set and its runs, the newest last.
type EvalSet struct {
	TrackerID string          `json:"trackerId"`
	Project   string          `json:"project"`
	Repo      string          `json:"repo"`
	Cases     []core.EvalCase `json:"cases"`
	Runs      []core.EvalRun  `json:"runs"`
}

// Limits of what an eval set keeps.
const (
	evalRunsKept  = 20      // the runs of a set; older ones are dropped
	evalTestsMax  = 1 << 20 // the oracle's test files of a case, bytes
	evalDiffMax   = 1 << 20 // a case's merged diff kept to show
	evalCandidate = 50      // merged tasks offered to add
	// evalCaseCost: the ceiling of a run per case when none is given, USD (as the watch's MaxCost of a task).
	evalCaseCost = config.DefaultWatchMaxCost
)

func (d *Dispatcher) evalFile() string { return filepath.Join(d.Config().StateDir, "evals.json") }

// readEvals: the sets kept, by navKey; the caller holds evalMu. A file that cannot be read is kept aside (and told
// to Mako's errors) and the sets start empty.
func (d *Dispatcher) readEvals() map[string]*EvalSet {
	d.evalReads++
	all := map[string]*EvalSet{}
	b, err := os.ReadFile(d.evalFile())
	if err != nil {
		return all
	}
	if err := json.Unmarshal(b, &all); err != nil || all == nil {
		if err == nil {
			err = errors.New("no sets in it")
		}
		to, rerr := keepAsideBroken(d.evalFile())
		if rerr == nil {
			err = fmt.Errorf("%w; kept as %s", err, to)
		}
		// Not in a goroutine: Fault takes faultsMu and the journal, never evalMu, and a write that outlives its caller
		// lands in a state folder that may be gone (a test's).
		d.Fault("mako", "evals.json cannot be read", err)
		return map[string]*EvalSet{}
	}
	for key, s := range all {
		if s == nil {
			delete(all, key)
			continue
		}
		// A run that says it is running but has no worker in this Mako: it stopped with the Mako it ran in.
		for i := range s.Runs {
			if r := &s.Runs[i]; r.State == core.EvalRunning && !d.evalLive(key, r.ID) {
				r.State, r.Reason = core.EvalStopped, "Mako restarted"
				skipRest(r, r.Reason)
			}
		}
	}
	return all
}

// evalCached is the parsed evals.json as it was when the file had this modification time and size.
type evalCached struct {
	mtime time.Time
	size  int64
	sets  map[string]*EvalSet
}

// evalSnapshot: the sets kept, for reading only (the caller holds evalMu and changes nothing in them). The board
// asks for them with every /api/state, and the file holds the tests of each case and the reviewers' notes of each
// run: it is parsed again only when it changed. Changes go through updateEval, which parses afresh.
func (d *Dispatcher) evalSnapshot() map[string]*EvalSet {
	info, err := os.Stat(d.evalFile())
	if err != nil {
		d.evalSeen = nil
		return map[string]*EvalSet{}
	}
	if c := d.evalSeen; c != nil && c.size == info.Size() && c.mtime.Equal(info.ModTime()) {
		return c.sets
	}
	sets := d.readEvals()
	d.evalSeen = &evalCached{mtime: info.ModTime(), size: info.Size(), sets: sets}
	if _, err := os.Stat(d.evalFile()); err != nil { // kept aside as broken: nothing to remember it by
		d.evalSeen = nil
	}
	return sets
}

// updateEval changes the set of key in evals.json; change returns an error to leave the file as it is.
func (d *Dispatcher) updateEval(key string, change func(s *EvalSet) error) (EvalSet, error) {
	d.evalMu.Lock()
	defer d.evalMu.Unlock()
	d.evalSeen = nil
	all := d.readEvals()
	s := all[key]
	if s == nil {
		s = &EvalSet{}
	}
	if err := change(s); err != nil {
		return *s, err
	}
	if len(s.Runs) > evalRunsKept {
		s.Runs = slices.Clone(s.Runs[len(s.Runs)-evalRunsKept:])
	}
	all[key] = s
	if len(s.Cases) == 0 && len(s.Runs) == 0 {
		delete(all, key)
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return *s, err
	}
	if err := os.MkdirAll(filepath.Dir(d.evalFile()), 0o755); err != nil {
		return *s, err
	}
	return *s, fsx.WriteFile(d.evalFile(), b, 0o600)
}

// evalSet: the set of key as it is kept.
func (d *Dispatcher) evalSet(key string) EvalSet {
	d.evalMu.Lock()
	defer d.evalMu.Unlock()
	if s := d.evalSnapshot()[key]; s != nil {
		return *s
	}
	return EvalSet{}
}

var unsafeSlug = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// evalSlug: the task's id as a name for files and refs ("group/app#12" → "group-app-12").
func evalSlug(id string) string {
	return strings.Trim(unsafeSlug.ReplaceAllString(id, "-"), "-.")
}

func (d *Dispatcher) evalDiffFile(taskID string) string {
	return filepath.Join(d.Config().StateDir, "evals", evalSlug(taskID)+".diff")
}

// evalRef: the clone's ref that keeps a case's head once its branch is gone.
func evalRef(taskID string) string { return "refs/mako/eval/" + evalSlug(taskID) }

// gitBytes runs git in dir and returns what it printed as it is (gitx.Run trims it: a file's content must stay whole).
func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.Bytes(), nil
}

func gitLine(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := gitBytes(ctx, dir, args...)
	return strings.TrimSpace(string(b)), err
}

// evalRepo: the tracker and the repository of a task, as the settings have them now.
func (d *Dispatcher) evalRepo(task core.Task) (trackers.Tracker, *config.Repo, error) {
	t := d.tracker(task.TrackerID)
	if t == nil {
		return nil, nil, fmt.Errorf("tracker %s was removed from the settings", task.TrackerID)
	}
	repo, err := d.route(t, task)
	if err != nil {
		return nil, nil, err
	}
	if !exists(repo.Path) {
		return nil, nil, fmt.Errorf("there is no clone of %s at %s", repo.Name, repo.Path)
	}
	return t, repo, nil
}

// AddEval keeps the merged task id in its repository's eval set: the commit its branch started from, what its MR
// changed and the tests it added or changed, read from the code host's MR and the clone. Added again, the case is
// read anew.
func (d *Dispatcher) AddEval(ctx context.Context, id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if r.ReviewState != "merged" || r.PRURL == "" {
		return errors.New("only a task whose MR is merged can go to eval")
	}
	t, repo, err := d.evalRepo(r.Task)
	if err != nil {
		return err
	}
	host, _, err := d.host(ctx, t, repo)
	if err != nil {
		return err
	}
	rh, ok := host.(trackers.RefsHost)
	if !ok {
		return fmt.Errorf("%s does not tell an MR's commits", host.Host())
	}
	refs, err := rh.MergedRefs(ctx, r.PRURL)
	if err != nil {
		return err
	}
	c, diff, err := snapshot(ctx, repo, r, refs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.evalDiffFile(id)), 0o755); err != nil {
		return err
	}
	if err := fsx.WriteFile(d.evalDiffFile(id), diff, 0o600); err != nil {
		return err
	}
	key := navKey(r.Task.TrackerID, r.Task.Project, repo.Name)
	_, err = d.updateEval(key, func(s *EvalSet) error {
		s.TrackerID, s.Project, s.Repo = r.Task.TrackerID, r.Task.Project, repo.Name
		s.Cases = slices.DeleteFunc(s.Cases, func(x core.EvalCase) bool { return x.TaskID == id })
		s.Cases = append(s.Cases, c)
		return nil
	})
	if err == nil {
		log.Printf("[%s] added to the eval set of %s: base %.10s, %d test files", id, repo.Name, c.Base, len(c.Tests))
	}
	return err
}

// snapshot reads the case of the merged task r from the clone: the MR's head is fetched under evalRef, its base is
// where the branch left the base branch; the diff between them, its size and its test files. The diff comes back
// cut to evalDiffMax.
func snapshot(ctx context.Context, repo *config.Repo, r core.Record, refs trackers.MergedRefs) (core.EvalCase, []byte, error) {
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ref := evalRef(r.ID)
	if _, err := gitBytes(fctx, repo.Path, "fetch", "--quiet", "origin", "+"+refs.Ref+":"+ref); err != nil {
		return core.EvalCase{}, nil, fmt.Errorf("the MR's commits could not be fetched: %w", err)
	}
	head := refs.Head
	if _, err := gitBytes(ctx, repo.Path, "cat-file", "-e", head+"^{commit}"); err != nil {
		if head, err = gitLine(ctx, repo.Path, "rev-parse", ref); err != nil {
			return core.EvalCase{}, nil, err
		}
	}
	if _, err := gitBytes(ctx, repo.Path, "cat-file", "-e", refs.Base+"^{commit}"); err != nil {
		// The base branch moved on in the clone's fetch: get it.
		gitBytes(fctx, repo.Path, "fetch", "--quiet", "origin")
	}
	base, err := gitLine(ctx, repo.Path, "merge-base", refs.Base, head)
	if err != nil || base == "" {
		return core.EvalCase{}, nil, fmt.Errorf("no commit the MR started from: %v", err)
	}
	c := core.EvalCase{TaskID: r.ID, Task: r.Task, PRURL: r.PRURL, Base: base, Head: head, Ref: ref, Cost: r.Cost, AddedAt: now()}
	c.Size = caseSize(r)
	if s, f := parseTime(r.StartedAt), parseTime(r.FinishedAt); !s.IsZero() && f.After(s) {
		c.Seconds = int(f.Sub(s).Seconds())
	}
	numstat, err := gitLine(ctx, repo.Path, "diff", "--numstat", base, head)
	if err != nil {
		return core.EvalCase{}, nil, err
	}
	c.Added, c.Removed, c.Files = sumNumstat(numstat)
	if c.Files == 0 {
		return core.EvalCase{}, nil, errors.New("the MR changed nothing")
	}
	status, err := gitLine(ctx, repo.Path, "diff", "--name-status", "--no-renames", base, head)
	if err != nil {
		return core.EvalCase{}, nil, err
	}
	size := 0
	for _, line := range strings.Split(status, "\n") {
		kind, path, ok := strings.Cut(line, "\t")
		if !ok || !core.IsTestFile(path) {
			continue
		}
		f := core.EvalFile{Path: path, Deleted: kind == "D"}
		if !f.Deleted {
			b, err := gitBytes(ctx, repo.Path, "show", head+":"+path)
			if err != nil {
				return core.EvalCase{}, nil, err
			}
			if size+len(b) > evalTestsMax {
				c.TestsCut = true
				continue
			}
			size += len(b)
			f.Content = string(b)
		}
		c.Tests = append(c.Tests, f)
	}
	diff, err := gitBytes(ctx, repo.Path, "diff", base, head)
	if err != nil {
		return core.EvalCase{}, nil, err
	}
	if len(diff) > evalDiffMax {
		diff = append(diff[:evalDiffMax:evalDiffMax], []byte("\n… (cut)\n")...)
	}
	return c, diff, nil
}

// sumNumstat: lines added and removed and the files of `git diff --numstat` (a binary file counts no lines).
func sumNumstat(out string) (added, removed, files int) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 {
			continue
		}
		files++
		a, _ := strconv.Atoi(f[0])
		r, _ := strconv.Atoi(f[1])
		added += a
		removed += r
	}
	return added, removed, files
}

// caseSize: the task's size, the assessment's after the work, else the estimate's.
func caseSize(r core.Record) string {
	if r.Assessment != nil && r.Assessment.Size != "" {
		return r.Assessment.Size
	}
	if r.Estimate != nil {
		return r.Estimate.Size
	}
	return ""
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// RemoveEval drops the task from the eval set of key, with its diff and its ref in the clone.
func (d *Dispatcher) RemoveEval(key, id string) error {
	var gone core.EvalCase
	_, err := d.updateEval(key, func(s *EvalSet) error {
		i := slices.IndexFunc(s.Cases, func(c core.EvalCase) bool { return c.TaskID == id })
		if i < 0 {
			return errors.New("the task is not in the eval set")
		}
		gone = s.Cases[i]
		s.Cases = slices.Delete(slices.Clone(s.Cases), i, i+1)
		return nil
	})
	if err != nil {
		return err
	}
	os.Remove(d.evalDiffFile(id))
	if _, repo, err := d.evalRepo(gone.Task); err == nil && gone.Ref != "" {
		gitBytes(context.Background(), repo.Path, "update-ref", "-d", gone.Ref)
	}
	return nil
}

// EvalState is a repository's eval as the board shows it.
type EvalState struct {
	Key       string `json:"key"`
	TrackerID string `json:"trackerId"`
	Project   string `json:"project"`
	Repo      string `json:"repo"`
	// Checks: the repository has checks; without them a run measures only the reviewer, the cost and the size.
	Checks     bool            `json:"checks"`
	Cases      []EvalCaseView  `json:"cases"`
	Candidates []EvalCandidate `json:"candidates"`
	Runs       []EvalRunView   `json:"runs"`
	Running    bool            `json:"running"`
	// Compare: the baseline and the variant side by side, task by task; nil until there are two runs to compare.
	Compare *EvalComparison `json:"compare,omitempty"`
	// MaxCost: the run's ceiling when none is given, USD.
	MaxCost float64 `json:"maxCost"`
}

// EvalCaseView is a case without its tests' contents: their paths only.
type EvalCaseView struct {
	core.EvalCase
	TestFiles []string `json:"testFiles"`
}

// EvalRunView is a run with its numbers.
type EvalRunView struct {
	core.EvalRun
	Summary  core.EvalSummary `json:"summary"`
	Baseline bool             `json:"baseline"`
}

// Of a result the board gets what it shows: the reviewer's notes cut to what a tooltip holds and no tails of the
// failed commands' output, which the file keeps whole.
const evalNotesInView = 300

func trimResults(rs []core.EvalResult) []core.EvalResult {
	out := make([]core.EvalResult, len(rs))
	for i, r := range rs {
		r.Review = clipText(r.Review, evalNotesInView)
		r.Checks, r.Oracle = withoutTails(r.Checks), withoutTails(r.Oracle)
		out[i] = r
	}
	return out
}

func withoutTails(cs []core.CheckResult) []core.CheckResult {
	if cs == nil {
		return nil
	}
	out := slices.Clone(cs)
	for i := range out {
		out[i].Tail = ""
	}
	return out
}

// EvalComparison: two runs of a set, the one compared with (Base) and the other (Variant), by run id.
type EvalComparison struct {
	Base    string           `json:"base"`
	Variant string           `json:"variant"`
	Deltas  []core.EvalDelta `json:"deltas"`
}

// compareRuns: the runs base and variant of the set when it has them; else the variant is the last run that
// changes something, one that ran to its end first (the last run at all when none changes anything) and the base the last finished baseline before it (the
// run before it when there is none). nil with fewer than two runs.
func compareRuns(runs []core.EvalRun, base, variant string) *EvalComparison {
	find := func(id string) int {
		return slices.IndexFunc(runs, func(r core.EvalRun) bool { return r.ID == id && id != "" })
	}
	last := func(ok func(i int) bool) int {
		for i := len(runs) - 1; i >= 0; i-- {
			if ok(i) {
				return i
			}
		}
		return -1
	}
	b, v := find(base), find(variant)
	if b == v {
		v = -1
	}
	if v < 0 {
		v = last(func(i int) bool { return i != b && !runs[i].Variant.Baseline() && runs[i].State == core.EvalDone })
	}
	if v < 0 {
		v = last(func(i int) bool { return i != b && !runs[i].Variant.Baseline() })
	}
	if v < 0 {
		v = last(func(i int) bool { return i != b && (b < 0 || i > b) })
	}
	if v < 0 {
		v = last(func(i int) bool { return i != b })
	}
	if b < 0 {
		b = last(func(i int) bool {
			return i < v && runs[i].Variant.Baseline() && runs[i].State != core.EvalRunning
		})
	}
	if b < 0 {
		b = last(func(i int) bool { return i < v })
	}
	if b < 0 {
		b = last(func(i int) bool { return i != v })
	}
	if b < 0 || v < 0 || b == v {
		return nil
	}
	return &EvalComparison{Base: runs[b].ID, Variant: runs[v].ID, Deltas: core.CompareEval(runs[b], runs[v])}
}

// EvalCandidate is a merged task that may go to the eval set; Suggested: a small or medium task whose checks
// passed (any, when the repository has no checks), the kind a run measures best.
type EvalCandidate struct {
	TaskID    string `json:"taskId"`
	Title     string `json:"title"`
	Size      string `json:"size,omitempty"`
	MergedAt  string `json:"mergedAt,omitempty"`
	Suggested bool   `json:"suggested"`
}

// Evals: the eval of each repository of project that has merged tasks or an eval set, for the board; base and
// variant: the runs to compare ("" the last baseline and the last variant).
func (d *Dispatcher) Evals(project, base, variant string) []EvalState {
	if project == "" {
		return nil
	}
	repos := d.navRepos(project)
	d.evalMu.Lock()
	kept := d.evalSnapshot()
	d.evalMu.Unlock()
	out := []EvalState{}
	keys := map[string]bool{}
	for key := range repos {
		keys[key] = true
	}
	for key, s := range kept {
		if s.Project == project {
			keys[key] = true
		}
	}
	for key := range keys {
		s := kept[key]
		if s == nil {
			s = &EvalSet{}
		}
		e := EvalState{Key: key, TrackerID: s.TrackerID, Project: project, Repo: s.Repo, Cases: []EvalCaseView{}, Candidates: []EvalCandidate{}, Runs: []EvalRunView{}}
		in := map[string]bool{}
		for _, c := range s.Cases {
			v := EvalCaseView{EvalCase: c, TestFiles: []string{}}
			for _, f := range c.Tests {
				v.TestFiles = append(v.TestFiles, f.Path)
			}
			v.Tests = nil
			e.Cases = append(e.Cases, v)
			in[c.TaskID] = true
		}
		e.MaxCost = evalCaseCost * float64(max(len(s.Cases), 1))
		for _, r := range s.Runs {
			sum := core.SummarizeEval(r)
			r.Results = trimResults(r.Results) // the kept run is shared: a copy goes out
			e.Runs = append(e.Runs, EvalRunView{EvalRun: r, Summary: sum, Baseline: r.Variant.Baseline()})
			if r.State == core.EvalRunning {
				e.Running = true
			}
		}
		e.Compare = compareRuns(s.Runs, base, variant)
		if x := repos[key]; x != nil {
			e.TrackerID, e.Repo, e.Checks = x.tracker, x.repo.Name, len(x.repo.Checks) > 0
			for _, r := range x.records {
				if in[r.ID] || r.PRURL == "" {
					continue
				}
				size := caseSize(r)
				// Green checks, where the repository has them: without them the merge is all there is to go by.
				green := !e.Checks || (r.Checks != nil && r.Checks.State == "passed")
				e.Candidates = append(e.Candidates, EvalCandidate{TaskID: r.ID, Title: r.Task.Title, Size: size, MergedAt: r.MergedAt,
					Suggested: (size == "small" || size == "medium") && green})
				if len(e.Candidates) == evalCandidate {
					break
				}
			}
			sort.SliceStable(e.Candidates, func(i, j int) bool { return e.Candidates[i].Suggested && !e.Candidates[j].Suggested })
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}
