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
// them, and its next ready branch gets another round, up to the settings' maximum. The board's «Зробити рев'ю»
// starts such a cycle of up to CycleRounds rounds whatever the settings say. Only Mako's own agents: Orca's
// cannot be written to.

// ReviewBranch runs the reviewer over the worktree; tests replace it.
var ReviewBranch = writer.AgentReview

// CycleRounds: the rounds with remarks a cycle started from the board's button gets at most.
const CycleRounds = 5

// reviewable: the task's agent is Mako's, with a worktree, and its branch is in work or in an open MR.
func reviewable(r core.Record) bool {
	if !onTerminal(r) || r.Worktree == "" || r.ArchivedAt != 0 {
		return false
	}
	return r.Status == core.HandedOff || r.Status == core.Done && r.PRURL != "" && r.ReviewState == ""
}

// reviewWanted: the settings or your button ask for a reviewer, the task's agent is Mako's and its rounds are
// not used up.
func (d *Dispatcher) reviewWanted(r core.Record) bool {
	if !reviewable(r) || !d.Config().Reviewer.Enabled && r.ReviewCycle == nil {
		return false
	}
	list, max := d.rounds(r)
	return roundsLeft(list, max)
}

// rounds: the reviewer's rounds that count, and how many with remarks it may have: those of the cycle you
// started, else all of them by the settings.
func (d *Dispatcher) rounds(r core.Record) ([]core.AgentReview, int) {
	if c := r.ReviewCycle; c != nil {
		return r.AgentReviews[min(c.From, len(r.AgentReviews)):], c.Rounds
	}
	return r.AgentReviews, d.Config().Reviewer.MaxRounds
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

// seen: a round looked at the branch at head already. A stale «running» one, from before Mako restarted, does
// not count; nor, when you asked for the review (asked), does one skipped for the task's risk.
func seen(list []core.AgentReview, head string, asked bool) bool {
	for _, a := range list {
		if a.Commit == head && a.State != "running" && !(asked && a.State == "skipped") {
			return true
		}
	}
	return false
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

// ReviewNow is the board's «Зробити рев'ю»: the reviewer reads the branch now, whatever the settings and the
// task's risk, and again after each push of the agent's fixes, up to CycleRounds rounds with remarks.
func (d *Dispatcher) ReviewNow(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("немає такої задачі")
	}
	if !reviewable(r) || !exists(r.Worktree) {
		return errors.New("рецензент читає лише гілки агентів Mako, що в роботі чи у відкритому MR")
	}
	d.mu.Lock()
	busy := d.reviewing[id]
	d.mu.Unlock()
	if busy {
		return errors.New("рецензент уже переглядає гілку")
	}
	if !d.agentIdle(id) {
		return errors.New("агент ще працює: зробіть рев'ю, коли він закінчить")
	}
	wt, base, err := d.workOf(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	head, ready := readyBranch(ctx, wt, base, branchOf(r), d.Config().Orca.MRBy != "me" || r.PRURL != "")
	if !ready {
		return errors.New("гілка ще не готова до рев'ю: немає комітів, є незакомічене або її не запушено")
	}
	if seen(r.AgentReviews, head, true) {
		return errors.New("рецензент уже бачив цей коміт: агент не змінив гілку після останнього рев'ю")
	}
	d.Store.Update(id, func(r *core.Record) {
		from := len(r.AgentReviews)
		if from > 0 && (r.AgentReviews[from-1].State == "skipped" || r.AgentReviews[from-1].State == "running") {
			from-- // setReview puts the new round in its place
		}
		r.ReviewCycle = &core.ReviewCycle{From: from, Rounds: CycleRounds}
	})
	go d.review(id, true)
	return nil
}

// ReviewStep: what the board offers about the reviewer: "running" (it reads the branch), "fixing" (the agent
// works on its remarks), "ready" (a review can start: the agent is idle), "" (the task is not for it).
func (d *Dispatcher) ReviewStep(r core.Record) string {
	if !reviewable(r) {
		return ""
	}
	d.mu.Lock()
	busy := d.reviewing[r.ID]
	d.mu.Unlock()
	last := ""
	if n := len(r.AgentReviews); n > 0 {
		last = r.AgentReviews[n-1].State
	}
	switch {
	case busy || last == "running" && !stale(r.AgentReviews[len(r.AgentReviews)-1].At):
		return "running"
	case !d.agentIdle(r.ID):
		if last == "notes" {
			return "fixing"
		}
		return ""
	}
	return "ready"
}

// maybeReview starts the reviewer on the task's branch when it is due: the agent's turn is over and the branch
// is ready and not reviewed at this commit, and the task is as risky as the settings ask for.
func (d *Dispatcher) maybeReview(id string) { d.review(id, false) }

// review runs the reviewer when it is due; manual: you asked for it (now, or by starting a cycle), whatever
// the task's risk.
func (d *Dispatcher) review(id string, manual bool) {
	// One at a time, and the record read after the one before ends: not a round on what it has replaced.
	d.mu.Lock()
	if d.reviewing[id] {
		d.mu.Unlock()
		return
	}
	d.reviewing[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.reviewing, id)
		d.mu.Unlock()
	}()
	r, ok := d.Store.Get(id)
	if !ok || !d.reviewWanted(r) || !exists(r.Worktree) || !d.agentIdle(id) {
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
	asked := manual || r.ReviewCycle != nil
	list, max := d.rounds(r)
	if !ready || !roundsLeft(list, max) || seen(r.AgentReviews, head, asked) {
		return
	}
	if !asked && !reviewed(r.AgentReviews) && cfg.Reviewer.When != config.ReviewAlways && cfg.Reviewer.When != "" {
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
	for _, a := range list {
		if a.State == "notes" {
			round++
		}
	}
	d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "running"})
	d.Log(id, fmt.Sprintf("рецензент переглядає гілку (раунд %d з %d)", round, max))
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
