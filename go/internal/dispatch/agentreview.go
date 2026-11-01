package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The reviewer agent: once the agent's branch is ready (it opened the MR, or ended its turn with the work
// committed and, unless you open MRs yourself, pushed), a second agent with a clean context reads the task and
// the branch's diff and writes its remarks. They go to the first agent as a message in its chat; it fixes
// them, and its next ready branch gets another round, up to the settings' maximum. Only Mako's own agents:
// Orca's cannot be written to.

// ReviewBranch runs the reviewer over the worktree; tests replace it.
var ReviewBranch = writer.AgentReview

// reviewWanted: the settings ask for a reviewer, the task's agent is Mako's and its rounds are not used up.
func (d *Dispatcher) reviewWanted(r core.Record) bool {
	cfg := d.Config().Reviewer
	if !cfg.Enabled || !onTerminal(r) || r.Worktree == "" || r.ArchivedAt != 0 {
		return false
	}
	if r.Status != core.HandedOff && !(r.Status == core.Done && r.PRURL != "" && r.ReviewState == "") {
		return false
	}
	return roundsLeft(r.AgentReviews, cfg.MaxRounds)
}

// roundsLeft: neither a clean review nor the maximum of rounds yet.
func roundsLeft(list []core.AgentReview, max int) bool {
	done := 0
	for _, a := range list {
		switch a.State {
		case "clean":
			return false
		case "notes":
			done++
		}
	}
	return done < max
}

// reviewDue: a round for the branch at head, unless one looked at it already (a stale «running» one, from
// before Mako restarted, does not count).
func reviewDue(list []core.AgentReview, head string, max int) bool {
	if !roundsLeft(list, max) {
		return false
	}
	for _, a := range list {
		if a.Commit == head && a.State != "running" {
			return false
		}
	}
	return true
}

// riskGate: whether the settings' When lets the reviewer read a task with this assessment. skip: why not (the
// task is less risky than asked); wait: its assessment is still running. No assessment, or a failed one, lets it.
func riskGate(when string, a *core.Assessment) (skip string, wait bool) {
	if when != config.ReviewMedium && when != config.ReviewHigh || a == nil {
		return "", false
	}
	if a.State == "running" {
		return "", !stale(a.At)
	}
	if a.State != "done" || a.Risk.Value == 0 {
		return "", false
	}
	switch v := a.Risk.Value; {
	case v <= 2:
		return "низький ризик", false
	case v == 3 && when == config.ReviewHigh:
		return "середній ризик", false
	}
	return "", false
}

// reviewed: the reviewer has read the branch already (not counting rounds it skipped): the next rounds do not
// look at the risk again.
func reviewed(list []core.AgentReview) bool {
	for _, a := range list {
		if a.State == "notes" || a.State == "clean" || a.State == "failed" {
			return true
		}
	}
	return false
}

// withoutSkipped: the rounds the reviewer did read.
func withoutSkipped(list []core.AgentReview) []core.AgentReview {
	out := []core.AgentReview{}
	for _, a := range list {
		if a.State != "skipped" {
			out = append(out, a)
		}
	}
	return out
}

// ReviewNow runs the reviewer the settings skipped for the task's low risk, from the board's button.
func (d *Dispatcher) ReviewNow(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("немає такої задачі")
	}
	if n := len(r.AgentReviews); n == 0 || r.AgentReviews[n-1].State != "skipped" {
		return errors.New("рев'ю агентом не пропускали")
	}
	if !d.reviewWanted(r) {
		return errors.New("рецензента вимкнено в налаштуваннях")
	}
	if !d.agentIdle(id) {
		return errors.New("агент ще працює: рецензент прочитає гілку, коли він закінчить")
	}
	d.inBackground(func() { d.review(id, true) })
	return nil
}

// maybeReview starts the reviewer on the task's branch when it is due: the agent's turn is over and the branch
// is ready and not reviewed at this commit, and the task is as risky as the settings ask for.
func (d *Dispatcher) maybeReview(id string) { d.review(id, false) }

// review runs the reviewer when it is due; manual: you asked for it, whatever the task's risk.
func (d *Dispatcher) review(id string, manual bool) {
	reviewable := func() (core.Record, bool) {
		r, ok := d.Store.Get(id)
		return r, ok && d.reviewWanted(r) && exists(r.Worktree) && d.agentIdle(id)
	}
	// Only a call with a review to do holds the task (autofix and autorebase wait while it does).
	if _, ok := reviewable(); !ok {
		return
	}
	// One at a time. Yours waits for the one running: the button must not be lost; the agent's turn starts
	// another call anyway.
	for {
		d.mu.Lock()
		if !d.reviewing[id] {
			d.reviewing[id] = true
			d.mu.Unlock()
			break
		}
		d.mu.Unlock()
		if !manual {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer func() {
		d.mu.Lock()
		delete(d.reviewing, id)
		d.mu.Unlock()
	}()
	// Read again: the one before may have reviewed this commit.
	r, ok := reviewable()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	wt, base, err := d.workOf(id)
	if err != nil {
		return
	}
	cfg := d.Config()
	push := cfg.Orca.MRBy != "me" || r.PRURL != ""
	head, ready := readyBranch(ctx, wt, base, branchOf(r), push)
	list := r.AgentReviews
	if manual {
		list = withoutSkipped(list)
	}
	if !ready || !reviewDue(list, head, cfg.Reviewer.MaxRounds) {
		return
	}
	if !manual && !reviewed(r.AgentReviews) && cfg.Reviewer.When != config.ReviewAlways && cfg.Reviewer.When != "" {
		// The risk comes from the assessment, made once the MR is found: when the agent opens it, wait for it.
		if r.PRURL == "" && cfg.Orca.MRBy != "me" {
			return
		}
		skip, wait := riskGate(cfg.Reviewer.When, r.Assessment)
		if wait {
			return
		}
		if skip != "" {
			d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "skipped", Reason: skip})
			d.Log(id, "рев'ю агентом пропущено: "+skip)
			return
		}
	}
	round := 1
	for _, a := range r.AgentReviews {
		if a.State == "notes" {
			round++
		}
	}
	d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "running"})
	d.Log(id, fmt.Sprintf("рецензент переглядає гілку (раунд %d з %d)", round, cfg.Reviewer.MaxRounds))
	model := firstOf(cfg.Reviewer.Model, r.Model, cfg.Terminal.Model)
	notes, err := ReviewBranch(ctx, wt, base, r.Task, model)
	link := d.BoardURL + "/#" + id
	switch {
	case err != nil:
		d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "failed", Error: err.Error()})
		d.Fault(id, "рецензент не впорався", err)
	case notes == "":
		d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "clean"})
		d.Log(id, "рецензент не має зауважень: рев'ю агентом пройдено")
		notify.Inform(notify.AgentReview, "🔍 "+id+": рев'ю агентом пройдено", r.Task.Title, link)
	default:
		if err := d.tellAgent(id, reviewRequest(notes, branchOf(r), push)); err != nil {
			d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "failed", Notes: notes, Error: "зауваження не передано агенту: " + err.Error()})
			d.Fault(id, "зауваження рецензента не передано агенту", err)
			return
		}
		d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "notes", Notes: notes})
		d.Log(id, "рецензент має зауваження, їх передано агенту")
		notify.Inform(notify.AgentReview, "🔍 "+id+": зауваження рецензента", clipText(firstLine(notes), 140), link)
	}
}

// setReview puts the round in the task's record: in place of its «running» or «skipped» one, or as a new one.
func (d *Dispatcher) setReview(id string, a core.AgentReview) {
	d.Store.Update(id, func(r *core.Record) {
		if n := len(r.AgentReviews); n > 0 && (r.AgentReviews[n-1].State == "running" || r.AgentReviews[n-1].State == "skipped") {
			// A new slice, not a write in place: the copies Store.Get gave out share the old one.
			r.AgentReviews = append(r.AgentReviews[:n-1:n-1], a)
			return
		}
		r.AgentReviews = append(r.AgentReviews, a)
	})
}

// agentIdle: the task's agent is not in the middle of a turn, nor waiting for your answer.
func (d *Dispatcher) agentIdle(id string) bool {
	if s := d.Chats.Get(id); s != nil && s.Running() {
		st, _, _ := s.State()
		return st == "done"
	}
	if s := d.Sessions.Get(id); s != nil && s.Running() {
		return s.State().State == "done"
	}
	return true // not running: the message resumes its conversation
}

// tellAgent gives the agent Mako's message (not yours: no lesson is drawn from it), resuming it if it is closed.
func (d *Dispatcher) tellAgent(id, text string) error {
	if d.Live(id) {
		return d.sayTo(id, text)
	}
	return d.resume(id, text)
}

// readyBranch: the worktree's HEAD, and whether the branch is ready for a review: commits over the base, nothing
// uncommitted, and with pushed the remote branch at HEAD.
func readyBranch(ctx context.Context, wt, base, branch string, pushed bool) (string, bool) {
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	if err != nil || head == "" {
		return "", false
	}
	from := "origin/" + base
	if _, err := gitx.Run(ctx, wt, "rev-parse", "--verify", "--quiet", from); err != nil {
		from = base
	}
	n, err := gitx.Run(ctx, wt, "rev-list", "--count", from+"..HEAD")
	if c, _ := strconv.Atoi(strings.TrimSpace(n)); err != nil || c == 0 {
		return head, false
	}
	if dirty, err := gitx.Run(ctx, wt, "status", "--porcelain", "--untracked-files=no"); err != nil || strings.TrimSpace(dirty) != "" {
		return head, false
	}
	if pushed {
		remote, err := gitx.Run(ctx, wt, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
		if err != nil || strings.TrimSpace(remote) != head {
			return head, false
		}
	}
	return head, true
}

// reviewRequest: the reviewer's remarks, as Mako hands them to the agent.
func reviewRequest(notes, branch string, push bool) string {
	end := "закоміть"
	if push {
		end = "закоміть і запуш гілку " + branch
	}
	return "Рецензент (окремий агент, з чистим контекстом) переглянув задачу й diff гілки " + branch + " і має зауваження:\n\n" +
		strings.TrimSpace(notes) + "\n\n" +
		"Виправ те, що справді є проблемою; з чим не згоден — коротко поясни чому. Прожени тести, " + end + "."
}
