package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// An eval run: each case of the set in a worktree of its own on the commit the task started from, the agent with no
// person (its plan, when the settings start the task with one, approved as it is), then the repository's checks,
// the reviewer and the task's own tests over what it did. Nothing is pushed, the tracker is not touched, and the
// worktree goes when the case is over, however it ended.

// Headless is the agent of an eval run; tests replace it.
var Headless = writer.Headless

// guardEval gives the worktree dir hooks (in the folder hooks) that refuse any push; tests replace it.
var guardEval = func(ctx context.Context, dir, hooks string) error {
	if err := gitx.WriteNoPushHooks(hooks); err != nil {
		return err
	}
	return gitx.InstallHooks(ctx, dir, hooks)
}

// evalAgentTimeout: the most one call of the agent may take.
var evalAgentTimeout = time.Hour

// evalSetupTimeout: the most the worktree's setup may take.
const evalSetupTimeout = 15 * time.Minute

// evalMinBudget: less than this left under the ceiling is no budget for another call of the agent, USD.
const evalMinBudget = 0.01

// evalClaudeMDMax: the most of a CLAUDE.md a variant may bring.
const evalClaudeMDMax = 256 << 10

// evalLiveRun is a run going on in this Mako.
type evalLiveRun struct {
	id     string
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// Why a run stopped before its end.
var (
	errEvalCeiling = errors.New("cost ceiling reached")
	errEvalStopped = errors.New("stopped by you")
)

// evalLive: the run id of key goes on in this Mako.
func (d *Dispatcher) evalLive(key, id string) bool {
	d.evalRunMu.Lock()
	defer d.evalRunMu.Unlock()
	l := d.evalRuns[key]
	return l != nil && l.id == id
}

var (
	modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,63}$`)
	branchArg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// StartEval starts a run of the eval set of key with the variant in the background; maxCost: its ceiling, USD (0:
// evalCaseCost a case). It returns the run's id.
func (d *Dispatcher) StartEval(ctx context.Context, key string, v core.EvalVariant, maxCost float64) (string, error) {
	v.Name, v.Model, v.Effort = strings.TrimSpace(v.Name), strings.TrimSpace(v.Model), strings.TrimSpace(v.Effort)
	v.ClaudeMDFrom, v.Instructions = strings.TrimSpace(v.ClaudeMDFrom), strings.TrimSpace(v.Instructions)
	if v.Model != "" && !modelName.MatchString(v.Model) {
		return "", fmt.Errorf("%q is not a model name", v.Model)
	}
	if v.Effort != "" && !slices.Contains(core.Efforts, v.Effort) {
		return "", fmt.Errorf("the effort is one of %s", strings.Join(core.Efforts, ", "))
	}
	set := d.evalSet(key)
	if len(set.Cases) == 0 {
		return "", errors.New("the eval set is empty: add merged tasks to it first")
	}
	_, repo, err := d.evalRepo(set.Cases[0].Task)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(v.ClaudeMD) == "" {
		v.ClaudeMD = ""
		if v.ClaudeMDFrom != "" {
			if v.ClaudeMD, err = claudeMDFrom(ctx, repo, v.ClaudeMDFrom); err != nil {
				return "", err
			}
		}
	} else if v.ClaudeMDFrom == "" {
		v.ClaudeMDFrom = "text"
	}
	if len(v.ClaudeMD) > evalClaudeMDMax {
		return "", fmt.Errorf("the CLAUDE.md is over %d KB", evalClaudeMDMax>>10)
	}
	if maxCost <= 0 {
		maxCost = evalCaseCost * float64(len(set.Cases))
	}
	run := core.EvalRun{ID: strconv.FormatInt(time.Now().UnixMilli(), 36), Variant: v, MaxCost: maxCost, State: core.EvalRunning, StartedAt: now()}
	for _, c := range set.Cases {
		run.Results = append(run.Results, core.EvalResult{TaskID: c.TaskID})
	}
	rctx, cancel := context.WithCancelCause(context.Background())
	live := &evalLiveRun{id: run.ID, cancel: cancel, done: make(chan struct{})}
	d.evalRunMu.Lock()
	if d.evalRuns == nil {
		d.evalRuns = map[string]*evalLiveRun{}
	}
	if d.evalRuns[key] != nil {
		d.evalRunMu.Unlock()
		cancel(nil)
		return "", errors.New("an eval run is already going on in this repository")
	}
	d.evalRuns[key] = live
	d.evalRunMu.Unlock()
	if _, err := d.updateEval(key, func(s *EvalSet) error { s.Runs = append(s.Runs, run); return nil }); err != nil {
		d.endEval(key, live)
		return "", err
	}
	d.sweepEval(repo)
	go d.evalRun(rctx, key, repo, run, set.Cases, live)
	return run.ID, nil
}

// StopEval stops the run of key: the agents are stopped, the cases not done are skipped, the worktrees go.
func (d *Dispatcher) StopEval(key string) error {
	d.evalRunMu.Lock()
	l := d.evalRuns[key]
	d.evalRunMu.Unlock()
	if l == nil {
		return errors.New("no eval run is going on")
	}
	l.cancel(errEvalStopped)
	select {
	case <-l.done:
	case <-time.After(30 * time.Second):
	}
	return nil
}

func (d *Dispatcher) endEval(key string, l *evalLiveRun) {
	l.cancel(nil)
	d.evalRunMu.Lock()
	if d.evalRuns[key] == l {
		delete(d.evalRuns, key)
	}
	d.evalRunMu.Unlock()
	close(l.done)
}

// claudeMDFrom: the CLAUDE.md of a variant from a file on this Mac ("/…", "~/…") or the root of a branch of the
// repository's origin.
func claudeMDFrom(ctx context.Context, repo *config.Repo, from string) (string, error) {
	if strings.HasPrefix(from, "/") || strings.HasPrefix(from, "~/") {
		path := from
		if strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, path[2:])
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("no CLAUDE.md at %s", from)
		}
		if info.Size() > evalClaudeMDMax {
			return "", fmt.Errorf("%s is over %d KB", from, evalClaudeMDMax>>10)
		}
		b, err := os.ReadFile(path)
		return string(b), err
	}
	if !branchArg.MatchString(from) {
		return "", fmt.Errorf("%q is neither a branch nor a file path", from)
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := gitBytes(fctx, repo.Path, "fetch", "--quiet", "origin", from); err != nil {
		return "", fmt.Errorf("no branch %s in origin: %w", from, err)
	}
	b, err := gitBytes(ctx, repo.Path, "show", "FETCH_HEAD:CLAUDE.md")
	if err != nil {
		return "", fmt.Errorf("the branch %s has no CLAUDE.md", from)
	}
	return string(b), nil
}

func (d *Dispatcher) evalDir() string { return filepath.Join(d.Config().StateDir, "eval-wt") }

// sweepEval removes what runs this Mako no longer has left behind: their folders and the clone's worktrees and
// branches of them.
func (d *Dispatcher) sweepEval(repo *config.Repo) {
	d.evalRunMu.Lock()
	live := map[string]bool{}
	for _, l := range d.evalRuns {
		live[l.id] = true
	}
	d.evalRunMu.Unlock()
	entries, _ := os.ReadDir(d.evalDir())
	for _, e := range entries {
		if !live[e.Name()] {
			os.RemoveAll(filepath.Join(d.evalDir(), e.Name()))
		}
	}
	ctx := context.Background()
	gitx.Run(ctx, repo.Path, "worktree", "prune")
	out, _ := gitx.Run(ctx, repo.Path, "for-each-ref", "--format=%(refname:short)", "refs/heads/mako-eval/")
	for _, b := range strings.Fields(out) {
		parts := strings.Split(b, "/")
		if len(parts) >= 2 && !live[parts[1]] {
			gitx.Run(ctx, repo.Path, "branch", "-D", b)
		}
	}
}

// evalRun runs the cases, at most as many at once as the WIP limit lets agents work, and keeps each result as
// it comes; the ceiling or StopEval stop the rest.
func (d *Dispatcher) evalRun(ctx context.Context, key string, repo *config.Repo, run core.EvalRun, cases []core.EvalCase, live *evalLiveRun) {
	defer d.endEval(key, live)
	defer os.RemoveAll(filepath.Join(d.evalDir(), run.ID))
	log.Printf("eval %s of %s: %d tasks, ceiling $%.2f", run.ID, key, len(cases), run.MaxCost)
	fctx, fcancel := context.WithTimeout(ctx, 2*time.Minute)
	gitx.Run(fctx, repo.Path, "fetch", "--quiet", "origin")
	fcancel()

	var mu sync.Mutex
	spent := 0.0
	b := &evalBudget{
		spend: func(usd float64) {
			mu.Lock()
			defer mu.Unlock()
			spent += usd
			if spent >= run.MaxCost {
				live.cancel(errEvalCeiling)
			}
		},
		left: func() float64 {
			mu.Lock()
			defer mu.Unlock()
			return run.MaxCost - spent
		},
	}
	save := func(i int, res core.EvalResult) {
		mu.Lock()
		total := spent
		mu.Unlock()
		d.saveEvalRun(key, run.ID, func(r *core.EvalRun) {
			r.Spent = total
			if i < len(r.Results) {
				r.Results[i] = res
			}
		})
	}
	workers := d.Config().WipLimit
	if workers <= 0 {
		workers = config.AutoWip()
	}
	workers = max(min(workers, len(cases)), 1)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				c := cases[i]
				if ctx.Err() != nil {
					continue
				}
				save(i, core.EvalResult{TaskID: c.TaskID, State: core.EvalRunning})
				res := d.runCase(ctx, repo, run, c, b)
				if ctx.Err() != nil && res.State != core.EvalPassed && res.State != core.EvalFailed {
					res.State, res.Error = core.EvalSkipped, context.Cause(ctx).Error()
				}
				log.Printf("eval %s: %s %s ($%.2f)", run.ID, c.TaskID, res.State, res.Cost)
				save(i, res)
			}
		}()
	}
	for i := range cases {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	mu.Lock()
	total := spent
	mu.Unlock()
	d.saveEvalRun(key, run.ID, func(r *core.EvalRun) {
		r.Spent, r.FinishedAt = total, now()
		r.State = core.EvalDone
		if cause := context.Cause(ctx); cause != nil {
			r.State, r.Reason = core.EvalStopped, cause.Error()
		}
		skipRest(r)
	})
	log.Printf("eval %s of %s: over, $%.2f", run.ID, key, total)
}

// skipRest marks the cases of a run that ended before them as skipped.
func skipRest(r *core.EvalRun) {
	for i := range r.Results {
		if s := r.Results[i].State; s == "" || s == core.EvalRunning {
			r.Results[i].State = core.EvalSkipped
		}
	}
}

// saveEvalRun changes the run id of key's set.
func (d *Dispatcher) saveEvalRun(key, id string, change func(r *core.EvalRun)) {
	_, err := d.updateEval(key, func(s *EvalSet) error {
		for i := range s.Runs {
			if s.Runs[i].ID == id {
				change(&s.Runs[i])
				return nil
			}
		}
		return errors.New("the run is gone")
	})
	if err != nil {
		log.Printf("eval %s: not saved: %v", id, err)
	}
}

// evalBudget: what a run may still spend, and what its calls spent.
type evalBudget struct {
	spend func(usd float64)
	left  func() float64
}

// runCase does the case again in a worktree of its own and measures what came of it.
func (d *Dispatcher) runCase(ctx context.Context, repo *config.Repo, run core.EvalRun, c core.EvalCase, b *evalBudget) (res core.EvalResult) {
	start := time.Now()
	res = core.EvalResult{TaskID: c.TaskID}
	var mu sync.Mutex
	cost := 0.0
	defer func() {
		mu.Lock()
		res.Cost = cost
		mu.Unlock()
		res.Seconds = int(time.Since(start).Seconds())
	}()
	actx := writer.WithSpend(ctx, func(s writer.Spend) {
		mu.Lock()
		cost += s.USD
		mu.Unlock()
		b.spend(s.USD)
	})
	fail := func(err error) core.EvalResult {
		res.State, res.Error = core.EvalError, clipText(err.Error(), 400)
		return res
	}
	cfg := d.Config()
	dir, branch, drop, err := d.evalWorktree(ctx, repo, run.ID, c)
	if err != nil {
		return fail(err)
	}
	defer drop()
	if run.Variant.ClaudeMD != "" {
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(run.Variant.ClaudeMD), 0o644); err != nil {
			return fail(err)
		}
	}
	if cmd := setupOf(repo, cfg.Terminal, dir); cmd != "" {
		// A failed setup does not stop the agent, as for a task: the checks will tell.
		if ok, _, out := RunCheck(ctx, dir, cmd, evalSetupTimeout); !ok {
			log.Printf("eval %s: %s: the setup failed: %s", run.ID, c.TaskID, lastOutput(out, 3))
		}
	}
	rec := core.Record{ID: c.TaskID, Task: c.Task}
	size := c.Size
	if !slices.Contains(core.Sizes, size) {
		size = "medium"
	}
	routeOf := func(phase string) core.Route {
		r := routeAt(cfg, rec, phase, repo.RouteMode, size, size)
		if phase != core.PhaseReview {
			if run.Variant.Model != "" {
				r.Model = run.Variant.Model
			}
			if run.Variant.Effort != "" {
				r.Effort = run.Variant.Effort
			}
		}
		return r
	}
	prompt := d.evalPrompt(repo, c, run.Variant, branch)
	if wantsPlan(cfg.Terminal, c.Task, nil, false) {
		if b.left() < evalMinBudget {
			return fail(errEvalCeiling)
		}
		plan, err := Headless(actx, dir, prompt+"\n\n"+i18n.TA("agent.evalPlanAsk"), writer.HeadlessOpts{Route: routeOf(core.PhasePlan), Plan: true, Budget: b.left(), Timeout: evalAgentTimeout})
		if err != nil {
			return fail(fmt.Errorf("plan: %w", err))
		}
		res.Planned = true
		prompt += "\n\n" + i18n.TA("agent.evalPlan", plan)
	}
	if b.left() < evalMinBudget {
		return fail(errEvalCeiling)
	}
	work := routeOf(core.PhaseWork)
	res.Route = work.Label()
	if _, err := Headless(actx, dir, prompt, writer.HeadlessOpts{Route: work, Budget: b.left(), Timeout: evalAgentTimeout}); err != nil {
		return fail(fmt.Errorf("agent: %w", err))
	}
	if err := commitLeft(ctx, dir, c.TaskID); err != nil {
		return fail(err)
	}
	numstat, err := gitx.Run(ctx, dir, "diff", "--numstat", c.Base, "HEAD")
	if err != nil {
		return fail(err)
	}
	res.Added, res.Removed, res.Files = sumNumstat(numstat)
	if res.Files == 0 {
		res.State, res.Error = core.EvalFailed, "the agent changed nothing"
		return res
	}
	if len(repo.Checks) > 0 {
		res.Checks = runChecks(ctx, dir, repo.Checks, cfg.Terminal.CheckTimeout())
		res.ChecksOK = allOK(res.Checks)
	}
	if ctx.Err() != nil {
		return fail(context.Cause(ctx))
	}
	base := repo.BaseBranch
	if base == "" {
		base = gitx.DetectBaseBranch(repo.Path)
	}
	if notes, err := ReviewBranch(actx, dir, base, c.Task, routeOf(core.PhaseReview), nil); err != nil {
		res.Review = "the reviewer failed: " + clipText(err.Error(), 300)
	} else {
		a := judge(notes, nil, 1)
		for _, r := range a.Remarks {
			if r.Weight == core.Must {
				res.Must++
			} else {
				res.Should++
			}
		}
		res.Review = clipText(notes, 4000)
	}
	if len(c.Tests) > 0 && len(repo.Checks) > 0 {
		if err := overlay(dir, c.Tests); err != nil {
			return fail(err)
		}
		res.Oracle = runChecks(ctx, dir, testChecks(repo.Checks), cfg.Terminal.CheckTimeout())
		res.OracleOK = allOK(res.Oracle)
	}
	if ctx.Err() != nil {
		return fail(context.Cause(ctx))
	}
	res.Judge()
	return res
}

// evalWorktree: a worktree of the case on a branch of its own from the commit the task started from, whose hooks
// refuse any push; drop removes both, whatever is in them.
func (d *Dispatcher) evalWorktree(ctx context.Context, repo *config.Repo, runID string, c core.EvalCase) (dir, branch string, drop func(), err error) {
	slug := evalSlug(c.TaskID)
	dir = filepath.Join(d.evalDir(), runID, slug)
	hooks := dir + ".hooks"
	branch = "mako-eval/" + runID + "/" + slug
	drop = func() {
		bctx := context.Background()
		if err := gitx.DropWorktree(bctx, repo.Path, dir, branch); err != nil {
			os.RemoveAll(dir)
			gitx.Run(bctx, repo.Path, "worktree", "prune")
			gitx.Run(bctx, repo.Path, "branch", "-D", branch)
		}
		os.RemoveAll(hooks)
	}
	if _, err := gitx.Run(ctx, repo.Path, "cat-file", "-e", c.Base+"^{commit}"); err != nil {
		return "", "", nil, fmt.Errorf("the commit %.10s the task started from is not in the clone", c.Base)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", nil, err
	}
	if _, err := gitx.Run(ctx, repo.Path, "worktree", "add", "--quiet", "--no-track", "-b", branch, dir, c.Base); err != nil {
		drop()
		return "", "", nil, err
	}
	if err = guardEval(ctx, dir, hooks); err != nil {
		drop()
		return "", "", nil, fmt.Errorf("the worktree's push guard: %w", err)
	}
	return dir, branch, drop, nil
}

// evalPrompt: the task as the agent gets it, with the end of the prompt the settings (or the variant) give, and
// what an eval run is: nobody answers, nothing is pushed.
func (d *Dispatcher) evalPrompt(repo *config.Repo, c core.EvalCase, v core.EvalVariant, branch string) string {
	cfg := d.Config()
	name := c.Task.TrackerID
	if t := d.tracker(c.Task.TrackerID); t != nil {
		name = t.Name()
	}
	o := cfg.Orca
	if v.Instructions != "" {
		o.Instructions = v.Instructions
	}
	j := orca.Job{Task: c.Task, TrackerName: name, Repo: repo, Branch: branch, Checks: gateChecks(cfg, repo), Language: cfg.AgentLanguage}
	return strings.Join([]string{orca.Prompt(c.Task, name), i18n.AgentNote(cfg.AgentLanguage), orca.Instructions(o.Instructions, o.MRBy, j),
		i18n.TA("agent.evalRun", branch)}, "\n\n")
}

// commitLeft commits what the agent left uncommitted, so that the result is all in the branch.
func commitLeft(ctx context.Context, dir, taskID string) error {
	status, err := gitx.Run(ctx, dir, "status", "--porcelain")
	if err != nil || status == "" {
		return err
	}
	if _, err := gitx.Run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err = gitx.Run(ctx, dir, "-c", "user.name=Mako", "-c", "user.email=mako@localhost", "commit", "--quiet", "-m", taskID+": what the eval agent left uncommitted")
	return err
}

// overlay lays the task's own tests over the agent's result: each file as the merged MR had it, or gone.
func overlay(dir string, files []core.EvalFile) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range files {
		path := filepath.FromSlash(f.Path)
		if f.Deleted {
			if err := root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := root.WriteFile(path, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// testChecks: the checks that run the tests, all of them when none says so.
func testChecks(checks []string) []string {
	var out []string
	for _, c := range checks {
		if checkName(c) == "tests" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return checks
	}
	return out
}

// runChecks runs the commands in dir one after another, each up to timeout; a cancelled ctx stops the rest.
func runChecks(ctx context.Context, dir string, commands []string, timeout time.Duration) []core.CheckResult {
	var out []core.CheckResult
	for _, cmd := range commands {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		ok, timedOut, text := RunCheck(ctx, dir, cmd, timeout)
		r := core.CheckResult{Name: checkName(cmd), Command: cmd, OK: ok, TimedOut: timedOut, Seconds: int(time.Since(start).Seconds())}
		if !ok {
			r.Tail = lastOutput(text, 15)
		}
		out = append(out, r)
	}
	return out
}

func allOK(results []core.CheckResult) *bool {
	ok := len(results) > 0
	for _, r := range results {
		ok = ok && r.OK
	}
	return &ok
}
