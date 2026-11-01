package dispatch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The reviewer agent: a second agent with a clean context reads the task and the branch's diff and writes its
// remarks. It starts only from the board's «Рев'ю», once the agent's branch is ready (committed and, unless you
// open MRs yourself, pushed). Its remarks stay in the task for you to read; the board's «Виправити» hands them
// to the agent, and after its fixes you may ask for another round. Only Mako's own agents: Orca's cannot be
// written to.

// ReviewBranch runs the reviewer over the worktree; tests replace it.
var ReviewBranch = writer.AgentReview

// reviewable: the task's agent is Mako's, with a worktree, and its branch is in work or in an open MR.
func reviewable(r core.Record) bool {
	if !onTerminal(r) || r.Worktree == "" || r.ArchivedAt != 0 {
		return false
	}
	return r.Status == core.HandedOff || r.Status == core.Done && r.PRURL != "" && r.ReviewState == ""
}

// seen: a round read the branch at head already. A stale «running» one, from before Mako restarted, one that
// failed, or one skipped for the task's risk (by older versions of Mako) does not count.
func seen(list []core.AgentReview, head string) bool {
	for _, a := range list {
		if a.Commit == head && a.State != "running" && a.State != "skipped" && a.State != "failed" {
			return true
		}
	}
	return false
}

// replaces: the next round over head goes in place of the last one: one that did not read the branch (still
// «running», or «skipped»), or one that failed on the same commit.
func replaces(list []core.AgentReview, head string) bool {
	n := len(list)
	if n == 0 {
		return false
	}
	last := list[n-1]
	return last.State == "running" || last.State == "skipped" || last.State == "failed" && last.Commit == head
}

// lastReview: the reviewer's last round over the task, the zero one if none.
func lastReview(r core.Record) core.AgentReview {
	if n := len(r.AgentReviews); n > 0 {
		return r.AgentReviews[n-1]
	}
	return core.AgentReview{}
}

// ReviewNow is the board's «Рев'ю»: the reviewer reads the branch now, one round.
func (d *Dispatcher) ReviewNow(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !reviewable(r) || !exists(r.Worktree) {
		return errors.New("the reviewer only reads Mako agent branches that are in progress or in an open MR")
	}
	if !d.agentIdle(id) {
		return errors.New("the agent is still working: review when it finishes")
	}
	wt, base, err := d.workOf(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	head, ready := readyBranch(ctx, wt, base, branchOf(r), d.pushes(r))
	if !ready {
		return errors.New("the branch is not ready for review: no commits, uncommitted changes, or not pushed")
	}
	if seen(r.AgentReviews, head) {
		return errors.New("the reviewer has already seen this commit: the agent has not changed the branch since the last review")
	}
	d.mu.Lock()
	busy := d.reviewing[id]
	d.reviewing[id] = true
	d.mu.Unlock()
	if busy {
		return errors.New("the reviewer is already reviewing the branch")
	}
	d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "running"})
	go d.review(id, wt, base, head)
	return nil
}

// SkipReview is the board's «Пропустити»: you decided the branch at its head needs no review. The round is kept
// as «skipped», which reads nothing: «Робити» can still start one, and its round goes in its place.
func (d *Dispatcher) SkipReview(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !reviewable(r) {
		return errors.New("review can only be skipped for Mako agents that are in progress or in an open MR")
	}
	d.mu.Lock()
	busy := d.reviewing[id]
	d.mu.Unlock()
	if busy {
		return errors.New("the reviewer is already reviewing the branch")
	}
	if last := lastReview(r); last.State == "notes" && !last.Sent {
		return errors.New("reviewer remarks are still pending: send them to the agent or leave them")
	}
	wt, _, err := d.workOf(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); err != nil || head == "" {
		return errors.New("could not read the branch")
	}
	d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "skipped", Reason: SkippedByYou})
	d.Log(id, "review skipped")
	return nil
}

// SkippedByYou: the reason of a round «Пропустити» left.
const SkippedByYou = "skipped by you"

// review runs the reviewer's round over head, which ReviewNow has marked as running; its remarks stay in the
// task until you hand them to the agent.
func (d *Dispatcher) review(id, wt, base, head string) {
	defer func() {
		d.mu.Lock()
		delete(d.reviewing, id)
		d.mu.Unlock()
	}()
	r, _ := d.Store.Get(id)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	d.Log(id, "reviewer is reviewing the branch")
	cfg := d.Config()
	model := firstOf(cfg.Reviewer.Model, r.Model, cfg.Terminal.Model)
	prior := core.OpenRemarks(r.AgentReviews)
	notes, err := ReviewBranch(d.spending(ctx, r.ID, core.PhaseReview), wt, base, r.Task, model, prior)
	link := d.BoardURL + "/#" + id
	if err != nil {
		d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "failed", Error: err.Error()})
		d.Fault(id, "reviewer failed", err)
		return
	}
	a := judge(notes, prior, core.NextRemark(r.AgentReviews))
	a.At, a.Commit = now(), head
	if a.State == "clean" {
		d.Log(id, "reviewer has no remarks: agent review passed")
		notify.Inform(notify.AgentReview, i18n.T("notify.reviewPassed", id), r.Task.Title, link)
	} else {
		d.Log(id, "reviewer left remarks: they are in the task, \"Fix\" will pass them to the agent")
		notify.Inform(notify.AgentReview, i18n.T("notify.reviewNotes", id), clipText(firstLine(notes), 140), link)
	}
	rounds := core.ReviewedRounds(r.AgentReviews)
	if a.Structured() || a.State == "clean" {
		rounds++
	}
	// The rounds ran out with remarks open: a person decides, told once; another round is theirs to start.
	a.Escalated = a.State == "notes" && a.Structured() && rounds >= core.ReviewRounds && !escalated(r.AgentReviews)
	d.setReview(id, a)
	pctx, pcancel := context.WithTimeout(context.Background(), time.Minute)
	defer pcancel()
	d.publishReview(pctx, id, rounds)
}

// judge: the round from the reviewer's answer: its numbered remarks (numbered from next), its verdicts on the
// earlier ones, and whether it is clean. An answer out of the format stays as it is, a round with remarks.
func judge(notes string, prior []core.Remark, next int) core.AgentReview {
	if notes == "" {
		// LGTM over earlier remarks: the reviewer was asked for it only when each of them is closed or accepted.
		a := core.AgentReview{State: "clean"}
		for _, p := range prior {
			a.Checks = append(a.Checks, core.Check{ID: p.ID, Verdict: core.Closed})
		}
		return a
	}
	known, byID := map[string]bool{}, map[string]core.Remark{}
	for _, p := range prior {
		known[p.ID], byID[p.ID] = true, p
	}
	a := core.AgentReview{
		Notes:   notes,
		Remarks: writer.Numbered(writer.ParseRemarks(notes), next),
		Checks:  writer.ParseChecks(notes, known),
	}
	// What it found still open goes to the agent again, under its number; so does a «must» it gave no verdict
	// on, when it kept to the format: it is not closed until a round says so.
	again := func(p core.Remark, why string) {
		p.Reply, p.ReplyNote, p.Again = "", "", true
		if why != "" {
			p.Text += "\n" + why
		}
		a.Remarks = append(a.Remarks, p)
	}
	checked := map[string]bool{}
	for _, c := range a.Checks {
		checked[c.ID] = true
		if c.Verdict == core.Open {
			why := ""
			if c.Note != "" {
				why = i18n.TA("review.open") + ": " + c.Note
			}
			again(byID[c.ID], why)
		}
	}
	if a.Structured() {
		for _, p := range prior {
			if !checked[p.ID] && p.Weight == core.Must {
				again(p, i18n.TA("review.unchecked"))
			}
		}
	}
	a.State = "notes"
	if a.Structured() && writer.Clean(a.Remarks) {
		a.State = "clean"
	}
	return a
}

// escalated: a round has already left the task to a person.
func escalated(rounds []core.AgentReview) bool {
	for _, a := range rounds {
		if a.Escalated {
			return true
		}
	}
	return false
}

// FixReview is the board's «Виправити»: the reviewer's last remarks go to the agent, once.
func (d *Dispatcher) FixReview(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !reviewable(r) {
		return errors.New("reviewer remarks can only be sent to Mako agents that are in progress or in an open MR")
	}
	if !exists(r.Worktree) {
		return errors.New("agent worktree was deleted: nobody to send the remarks to")
	}
	if !d.agentIdle(id) {
		return errors.New("the agent is still working: send the remarks when it finishes")
	}
	// Taken in the record first: a second press finds them sent.
	var last core.AgentReview
	d.Store.Update(id, func(r *core.Record) {
		n := len(r.AgentReviews)
		if n == 0 || r.AgentReviews[n-1].State != "notes" || r.AgentReviews[n-1].Sent {
			return
		}
		last = r.AgentReviews[n-1]
		r.AgentReviews = markSent(r.AgentReviews, true)
	})
	if last.State != "notes" {
		if l := lastReview(r); l.State == "notes" && l.Sent {
			return errors.New("remarks were already sent to the agent")
		}
		return errors.New("no reviewer remarks")
	}
	head := ""
	if wt, _, err := d.workOf(id); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		head, _ = gitx.Run(ctx, wt, "rev-parse", "HEAD")
		cancel()
	}
	d.Store.Update(id, func(r *core.Record) {
		if n := len(r.AgentReviews); n > 0 && r.AgentReviews[n-1].Commit == last.Commit {
			r.AgentReviews = withLast(r.AgentReviews, func(a *core.AgentReview) { a.SentHead = strings.TrimSpace(head) })
		}
	})
	d.mu.Lock()
	d.fixing[id] = time.Now()
	d.mu.Unlock()
	if err := d.tellAgent(id, reviewRequest(last, branchOf(r), d.pushes(r))); err != nil {
		d.mu.Lock()
		delete(d.fixing, id)
		d.mu.Unlock()
		d.Store.Update(id, func(r *core.Record) {
			if n := len(r.AgentReviews); n > 0 && r.AgentReviews[n-1].Commit == last.Commit {
				r.AgentReviews = markSent(r.AgentReviews, false)
			}
		})
		return errors.New("remarks not sent to the agent: " + err.Error())
	}
	d.Log(id, "reviewer remarks sent to the agent")
	return nil
}

// withLast: the rounds with the last one changed by f, in a new slice: the copies Store.Get gave out share the old.
func withLast(list []core.AgentReview, f func(*core.AgentReview)) []core.AgentReview {
	out := append([]core.AgentReview(nil), list...)
	last := out[len(out)-1]
	last.Remarks = append([]core.Remark(nil), last.Remarks...)
	f(&last)
	out[len(out)-1] = last
	return out
}

// markSent: the rounds with the last one's Sent set, in a new slice: the copies Store.Get gave out share the old.
func markSent(list []core.AgentReview, sent bool) []core.AgentReview {
	return withLast(list, func(a *core.AgentReview) { a.Sent = sent })
}

// ReviewStep: what the board offers about the reviewer: "running" (it reads the branch); "notes" (its remarks
// wait for your «Виправити»); "fixing" (the agent works on them); "ready" (the button can start a round: the
// agent is idle and the branch is ready and not read at this commit; after the rounds ran out, a person's
// choice); "idle" (the agent is idle, with nothing new for the reviewer); "" (the task is not for it).
func (d *Dispatcher) ReviewStep(r core.Record) string {
	if !reviewable(r) {
		return ""
	}
	last := lastReview(r)
	// A stale «running» round, from before Mako restarted, is no reviewer.
	d.mu.Lock()
	busy := d.reviewing[r.ID]
	d.mu.Unlock()
	if busy && last.State == "running" {
		return "running"
	}
	if !d.agentIdle(r.ID) {
		if last.State == "notes" && last.Sent {
			return "fixing"
		}
		return ""
	}
	if last.State == "notes" && !last.Sent {
		return "notes"
	}
	head, ready := d.lookBranch(r)
	if !ready || seen(r.AgentReviews, head) {
		return "idle"
	}
	return "ready"
}

// branchLook: the worktree's HEAD and whether its branch is ready for a review, as last looked at.
type branchLook struct {
	at    time.Time
	head  string
	ready bool
}

// lookBranch: readyBranch for the board, looked at once in 15 seconds so that its polls do not run git.
func (d *Dispatcher) lookBranch(r core.Record) (string, bool) {
	d.mu.Lock()
	l, ok := d.branches[r.ID]
	d.mu.Unlock()
	if ok && time.Since(l.at) < 15*time.Second {
		return l.head, l.ready
	}
	l = branchLook{at: time.Now()}
	if wt, base, err := d.workOf(r.ID); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		l.head, l.ready = readyBranch(ctx, wt, base, branchOf(r), d.pushes(r))
		cancel()
	}
	d.mu.Lock()
	d.branches[r.ID] = l
	d.mu.Unlock()
	return l.head, l.ready
}

// pushes: the branch must be pushed to be ready: the agent opens the MR, or there is one.
func (d *Dispatcher) pushes(r core.Record) bool { return d.Config().Orca.MRBy != "me" || r.PRURL != "" }

// setReview puts the round in the task's record: in place of its «running» or «skipped» one, or of one that
// failed on the same commit, or as a new one.
func (d *Dispatcher) setReview(id string, a core.AgentReview) {
	d.Store.Update(id, func(r *core.Record) {
		if replaces(r.AgentReviews, a.Commit) {
			// A new slice, not a write in place: the copies Store.Get gave out share the old one.
			n := len(r.AgentReviews)
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
	if dirty, err := gitx.Run(ctx, wt, "--no-optional-locks", "status", "--porcelain", "--untracked-files=no"); err != nil || strings.TrimSpace(dirty) != "" {
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

// reviewRequest: the reviewer's remarks, as Mako hands them to the agent: numbered, with the report it ends
// with; a round out of the format as the reviewer wrote it.
func reviewRequest(a core.AgentReview, branch string, push bool) string {
	end := i18n.TA("agent.reviewCommit")
	if push {
		end = i18n.TA("agent.reviewCommitPush", branch)
	}
	if len(a.Remarks) == 0 {
		return i18n.TA("agent.reviewNotes", branch) + "\n\n" + strings.TrimSpace(a.Notes) + "\n\n" + i18n.TA("agent.reviewFix", end)
	}
	return i18n.TA("agent.reviewNotes", branch) + "\n\n" + writer.RemarksList(a.Remarks) + "\n\n" + i18n.TA("agent.reviewFix", end) + "\n\n" + i18n.TA("agent.reviewReport")
}
