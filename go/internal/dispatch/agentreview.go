package dispatch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// maybeReview starts the reviewer on the task's branch when it is due: the agent's turn is over and the branch
// is ready and not reviewed at this commit.
func (d *Dispatcher) maybeReview(id string) {
	r, ok := d.Store.Get(id)
	if !ok || !d.reviewWanted(r) || !exists(r.Worktree) || !d.agentIdle(id) {
		return
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	wt, base, err := d.workOf(id)
	if err != nil {
		return
	}
	cfg := d.Config()
	push := cfg.Orca.MRBy != "me" || r.PRURL != ""
	head, ready := readyBranch(ctx, wt, base, branchOf(r), push)
	if !ready || !reviewDue(r.AgentReviews, head, cfg.Reviewer.MaxRounds) {
		return
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
		notify.Inform("🔍 "+id+": рев'ю агентом пройдено", r.Task.Title, link)
	default:
		if err := d.tellAgent(id, reviewRequest(notes, branchOf(r), push)); err != nil {
			d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "failed", Notes: notes, Error: "зауваження не передано агенту: " + err.Error()})
			d.Fault(id, "зауваження рецензента не передано агенту", err)
			return
		}
		d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "notes", Notes: notes})
		d.Log(id, "рецензент має зауваження, їх передано агенту")
		notify.Inform("🔍 "+id+": зауваження рецензента", clipText(firstLine(notes), 140), link)
	}
}

// setReview puts the round in the task's record: in place of its «running» one, or as a new one.
func (d *Dispatcher) setReview(id string, a core.AgentReview) {
	d.Store.Update(id, func(r *core.Record) {
		if n := len(r.AgentReviews); n > 0 && r.AgentReviews[n-1].State == "running" {
			r.AgentReviews[n-1] = a
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
