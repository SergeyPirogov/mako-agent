package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The checkpoint before the MR. A repo with checks in its settings (tests, lint, build) has Mako's agent only
// commit; once its turn is over, Mako runs the checks in its worktree itself. They passed: Mako pushes the
// branch and opens the MR. One failed: no MR, the agent gets the last lines of what failed to fix; after the
// settings' attempts the task needs you. Orca's agents and MRs you open yourself are as before.

// RunCheck runs one check in dir; tests replace it.
var RunCheck = runCheck

// reviewText writes the MR's title and description from the branch; tests replace it.
var reviewText = writer.ReviewText

// checkTailLines: how much of a failed check's output the agent and the card get.
const checkTailLines = 40

// checksFor: the repo whose checks stand before the task's MR, nil when Mako does not open it: the agent is
// Orca's, you open MRs yourself, the MR is there already, or the repo has no checks.
func (d *Dispatcher) checksFor(r core.Record) *config.Repo {
	if r.Status != core.HandedOff || !onTerminal(r) || r.PRURL != "" || r.Worktree == "" || r.ArchivedAt != 0 || d.Config().Orca.MRBy == "me" {
		return nil
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return nil
	}
	repo, err := d.route(t, r.Task)
	if err != nil || len(repo.Checks) == 0 {
		return nil
	}
	return repo
}

// gateChecks: the checks Mako's agent is told Mako runs before opening its MR; none when you open MRs yourself.
func gateChecks(cfg *config.Config, repo *config.Repo) []string {
	if cfg.Orca.MRBy == "me" || repo == nil {
		return nil
	}
	return repo.Checks
}

// maybeCheck runs the checks over the agent's branch when they are due: its turn is over, the work is committed
// and this commit was not checked yet. Passed checks open the MR.
func (d *Dispatcher) maybeCheck(id string) {
	// One at a time, and the record read after the one before ends: not a run on what it has replaced.
	d.mu.Lock()
	if d.checking[id] {
		d.mu.Unlock()
		return
	}
	d.checking[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.checking, id)
		d.mu.Unlock()
	}()
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	repo := d.checksFor(r)
	if repo == nil || !exists(r.Worktree) || !d.agentIdle(id) {
		return
	}
	term := d.Config().Terminal
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(repo.Checks))*term.CheckTimeout()+5*time.Minute)
	defer cancel()
	wt, base, err := d.workOf(id)
	if err != nil {
		return
	}
	head, ready := readyBranch(ctx, wt, base, branchOf(r), false)
	if !ready {
		return
	}
	if c := r.Checks; c != nil && c.Commit == head {
		// This commit is checked: a failed run waits for the next commit; a passed one whose MR did not open
		// tries again («running» is from before Mako restarted: run them again).
		if c.State == "passed" {
			d.openMR(ctx, id, repo, base)
		}
		if c.State != "running" {
			return
		}
	}
	d.Store.Update(id, func(rec *core.Record) {
		c := rec.Checks
		if c == nil {
			c = &core.Checks{}
		}
		c.At, c.Commit, c.State, c.Results, c.Error = now(), head, "running", nil, ""
		rec.Checks = c
	})
	d.Log(id, "перевірка перед MR: "+strings.Join(repo.Checks, "; "))
	results := make([]core.CheckResult, 0, len(repo.Checks))
	passed := true
	for _, command := range repo.Checks {
		start := time.Now()
		ok, timedOut, out := RunCheck(ctx, wt, command, term.CheckTimeout())
		res := core.CheckResult{Name: checkName(command), Command: command, OK: ok, TimedOut: timedOut, Seconds: int(time.Since(start).Seconds())}
		if !ok {
			passed = false
			res.Tail = lastOutput(out, checkTailLines)
		}
		results = append(results, res)
	}
	var fails int
	var attention bool
	d.Store.Update(id, func(rec *core.Record) {
		c := rec.Checks
		c.At, c.Results, c.State = now(), results, "passed"
		if passed {
			c.Fails, c.Attention = 0, false
		} else {
			c.State, c.Fails = "failed", c.Fails+1
			c.Attention = c.Attention || c.Fails >= term.CheckTries()
		}
		fails, attention = c.Fails, c.Attention
	})
	d.Log(id, "перевірка: "+checksLine(results))
	link := d.BoardURL + "/#" + id
	switch {
	case passed:
		d.openMR(ctx, id, repo, base)
	case attention:
		d.Log(id, fmt.Sprintf("перевірка не пройшла %d раз(и) поспіль: MR не відкрито, задача потребує уваги", fails))
		notify.Inform(notify.Stopped, i18n.T("notify.attention", id), i18n.T("notify.checksFailed", checksLine(results)), link)
	default:
		if err := d.tellAgent(id, checksRequest(results, fails, term.CheckTries())); err != nil {
			d.Fault(id, "результат перевірки не передано агенту", err)
		}
	}
}

// openMR pushes the checked branch and opens its MR; CheckHandoffs then finds it as if the agent had opened it.
func (d *Dispatcher) openMR(ctx context.Context, id string, repo *config.Repo, base string) {
	r, _ := d.Store.Get(id)
	fail := func(err error) {
		d.Store.Update(id, func(rec *core.Record) {
			if rec.Checks != nil {
				rec.Checks.Error = err.Error()
			}
		})
		d.Fault(id, "MR не відкрито", err)
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return
	}
	host, project, err := d.host(ctx, t, repo)
	if err != nil {
		fail(err)
		return
	}
	branch := branchOf(r)
	// Mako's own push goes past the agent's push guard: the same look for secrets first. «Це не секрет» on the
	// card lets the next round push; a new commit is checked again.
	if found, err := d.CheckSecrets(ctx, id); err == nil && len(found) > 0 {
		const held = "пуш зупинено: у змінах схоже на секрети"
		if r.Checks == nil || r.Checks.Error != held {
			if err := d.tellAgent(id, secretsRequest(found)); err != nil {
				d.Fault(id, "знахідки перевірки на секрети не передано агенту", err)
			}
		}
		d.Store.Update(id, func(rec *core.Record) {
			if rec.Checks != nil {
				rec.Checks.Error = held
			}
		})
		return
	}
	if _, err := gitx.Run(ctx, r.Worktree, "push", "-u", "origin", "HEAD:refs/heads/"+branch); err != nil {
		fail(err)
		return
	}
	if review, err := host.FindReview(ctx, project, branch); err == nil && review != nil && review.State == "open" {
		d.Wake() // opened meanwhile (by the agent after all)
		return
	}
	title, summary, err := reviewText(ctx, r.Worktree, base, r.Task)
	if err != nil || title == "" {
		title, summary = r.Task.ID+": "+r.Task.Title, ""
		if subject, err := gitx.Run(ctx, r.Worktree, "log", "-1", "--format=%s"); err == nil && subject != "" {
			title = subject
		}
	}
	review, err := host.OpenReview(ctx, project, branch, base, title, mrDescription(r, summary))
	if err != nil {
		fail(err)
		return
	}
	d.Store.Update(id, func(rec *core.Record) {
		if rec.Checks != nil {
			rec.Checks.Error = ""
		}
	})
	d.Log(id, "перевірка пройшла, Mako відкрив MR: "+review.URL)
	d.Wake()
}

// mrDescription: what changed, the checks that passed, and the closing reference.
func mrDescription(r core.Record, summary string) string {
	var b strings.Builder
	if summary = strings.TrimSpace(summary); summary != "" {
		b.WriteString(i18n.TA("mr.whatChanged") + "\n" + summary + "\n\n")
	}
	if r.Checks != nil && len(r.Checks.Results) > 0 {
		b.WriteString(i18n.TA("mr.howChecked") + "\n")
		for _, c := range r.Checks.Results {
			b.WriteString(fmt.Sprintf("- ✓ `%s`\n", c.Command))
		}
		b.WriteString("\n")
	}
	closing := r.Task.ClosingRef
	if closing == "" {
		closing = r.Task.URL
	}
	b.WriteString(closing + "\n\n" + fmt.Sprintf("🦈 Mako · [%s](%s)", r.Task.ID, r.Task.URL))
	return b.String()
}

// checksRequest: the failed checks, as Mako hands them to the agent.
func checksRequest(results []core.CheckResult, fails, tries int) string {
	var b strings.Builder
	b.WriteString(i18n.TA("agent.checksFailed", fails, tries, checksLine(results)) + "\n")
	for _, c := range results {
		if c.OK {
			continue
		}
		what := i18n.TA("agent.checkFell")
		if c.TimedOut {
			what = i18n.TA("agent.checkTimedOut", c.Seconds)
		}
		b.WriteString("\n" + i18n.TA("agent.checkTail", c.Command, what) + "\n```\n" + c.Tail + "\n```\n")
	}
	b.WriteString("\n" + i18n.TA("agent.checksFix"))
	return b.String()
}

// checksLine: «✓ тести, ✗ лінт».
func checksLine(results []core.CheckResult) string {
	parts := make([]string, len(results))
	for i, c := range results {
		mark := "✓"
		switch {
		case c.TimedOut:
			mark = "⏱"
		case !c.OK:
			mark = "✗"
		}
		parts[i] = mark + " " + c.Name
	}
	return strings.Join(parts, ", ")
}

var (
	testCmd  = regexp.MustCompile(`(?i)\btests?\b|jest|vitest|pytest|rspec|mocha|phpunit`)
	lintCmd  = regexp.MustCompile(`(?i)lint|\bvet\b|staticcheck|eslint|ruff|flake8|rubocop|clippy|\bfmt\b|prettier|tsc\b.*--noEmit|typecheck|type-check`)
	buildCmd = regexp.MustCompile(`(?i)\bbuild\b|compile|\bmake\b|tsc\b`)
)

// checkName: what a check is, for the card: «тести», «лінт», «збірка», or the command itself.
func checkName(command string) string {
	switch {
	case testCmd.MatchString(command):
		return "тести"
	case lintCmd.MatchString(command):
		return "лінт"
	case buildCmd.MatchString(command):
		return "збірка"
	}
	return clipText(command, 40)
}

// runCheck runs command with the shell in dir, at most timeout; with its children, it is killed on time.
func runCheck(ctx context.Context, dir, command string, timeout time.Duration) (ok, timedOut bool, out string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "CI=true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var buf tailBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	text := buf.String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, true, text + fmt.Sprintf("\n(перервано через %s)", timeout)
	}
	if err != nil {
		return false, false, text + "\n(" + err.Error() + ")"
	}
	return true, false, text
}

// tailBuffer keeps the last 64 KB written to it: a long test run is not kept whole.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

const tailKeep = 64 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 2*tailKeep {
		keep := append([]byte(nil), t.buf.Bytes()[t.buf.Len()-tailKeep:]...)
		t.buf.Reset()
		t.buf.Write(keep)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// lastOutput: the last n lines of a command's output, without the blank end.
func lastOutput(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
