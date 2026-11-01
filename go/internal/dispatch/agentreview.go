package dispatch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
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
		return errors.New("немає такої задачі")
	}
	if !reviewable(r) || !exists(r.Worktree) {
		return errors.New("рецензент читає лише гілки агентів Mako, що в роботі чи у відкритому MR")
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
	head, ready := readyBranch(ctx, wt, base, branchOf(r), d.pushes(r))
	if !ready {
		return errors.New("гілка ще не готова до рев'ю: немає комітів, є незакомічене або її не запушено")
	}
	if seen(r.AgentReviews, head) {
		return errors.New("рецензент уже бачив цей коміт: агент не змінив гілку після останнього рев'ю")
	}
	d.mu.Lock()
	busy := d.reviewing[id]
	d.reviewing[id] = true
	d.mu.Unlock()
	if busy {
		return errors.New("рецензент уже переглядає гілку")
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
		return errors.New("немає такої задачі")
	}
	if !reviewable(r) {
		return errors.New("рев'ю пропускається лише для агентів Mako, що в роботі чи у відкритому MR")
	}
	d.mu.Lock()
	busy := d.reviewing[id]
	d.mu.Unlock()
	if busy {
		return errors.New("рецензент уже переглядає гілку")
	}
	if last := lastReview(r); last.State == "notes" && !last.Sent {
		return errors.New("зауваження рецензента ще чекають: передайте їх агенту або залиште")
	}
	wt, _, err := d.workOf(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); err != nil || head == "" {
		return errors.New("не вдалось прочитати гілку")
	}
	d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "skipped", Reason: SkippedByYou})
	d.Log(id, "рев'ю пропущено")
	return nil
}

// SkippedByYou: the reason of a round «Пропустити» left.
const SkippedByYou = "пропущено вами"

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
	d.Log(id, "рецензент переглядає гілку")
	cfg := d.Config()
	model := firstOf(cfg.Reviewer.Model, r.Model, cfg.Terminal.Model)
	notes, err := ReviewBranch(d.spending(ctx, r.ID, core.PhaseReview), wt, base, r.Task, model)
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
		d.setReview(id, core.AgentReview{At: now(), Commit: head, State: "notes", Notes: notes})
		d.Log(id, "рецензент залишив зауваження: вони в задачі, «Виправити» передасть їх агенту")
		notify.Inform(notify.AgentReview, "🔍 "+id+": є зауваження рецензента", clipText(firstLine(notes), 140), link)
	}
}

// FixReview is the board's «Виправити»: the reviewer's last remarks go to the agent, once.
func (d *Dispatcher) FixReview(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("немає такої задачі")
	}
	if !reviewable(r) {
		return errors.New("зауваження рецензента передаються лише агентам Mako, що в роботі чи у відкритому MR")
	}
	if !exists(r.Worktree) {
		return errors.New("worktree агента видалено: зауваження нікому передати")
	}
	if !d.agentIdle(id) {
		return errors.New("агент ще працює: передайте зауваження, коли він закінчить")
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
			return errors.New("зауваження вже передано агенту")
		}
		return errors.New("немає зауважень рецензента")
	}
	if err := d.tellAgent(id, reviewRequest(last.Notes, branchOf(r), d.pushes(r))); err != nil {
		d.Store.Update(id, func(r *core.Record) {
			if n := len(r.AgentReviews); n > 0 && r.AgentReviews[n-1].Commit == last.Commit {
				r.AgentReviews = markSent(r.AgentReviews, false)
			}
		})
		return errors.New("зауваження не передано агенту: " + err.Error())
	}
	d.Log(id, "зауваження рецензента передано агенту")
	return nil
}

// markSent: the rounds with the last one's Sent set, in a new slice: the copies Store.Get gave out share the old.
func markSent(list []core.AgentReview, sent bool) []core.AgentReview {
	out := append([]core.AgentReview(nil), list...)
	out[len(out)-1].Sent = sent
	return out
}

// ReviewStep: what the board offers about the reviewer: "running" (it reads the branch); "notes" (its remarks
// wait for your «Виправити»); "fixing" (the agent works on them); "ready" (the button can start a round: the
// agent is idle and the branch is ready and not read at this commit); "idle" (the agent is idle, with nothing
// new for the reviewer); "" (the task is not for it).
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
