package dispatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// After «Виправити» the agent ends its turn with a report, a line per remark; Mako reads it into the round,
// asks once for it when there is none, and tells the board when the agent says «fixed» with no new commit.
// With the settings' agentReview.comments, the remarks and the report go to the tracker as comments.

// turnEnd: when the task's agent last changed its state, and whether it is idle now.
func (d *Dispatcher) turnEnd(id string) (time.Time, bool) {
	if s := d.Chats.Get(id); s != nil && s.Running() {
		st, _, since := s.State()
		return since, st == "done"
	}
	if s := d.Sessions.Get(id); s != nil && s.Running() {
		st := s.State()
		return st.Since, st.State == "done"
	}
	return time.Time{}, false
}

// awaitsReply: the task's last round went to the agent, whose report Mako has not read yet.
func awaitsReply(r core.Record) bool {
	last := lastReview(r)
	return last.State == "notes" && last.Sent && last.ReplyAt == "" && len(last.Remarks) > 0
}

// maybeReply reads the agent's report once its turn after «Виправити» is over. A task Mako restarted in
// between has no time of sending: its report is taken if the agent's last words have one, no asking.
func (d *Dispatcher) maybeReply(r core.Record) {
	if !awaitsReply(r) || !onTerminal(r) {
		return
	}
	d.mu.Lock()
	sent, known := d.fixing[r.ID]
	busy := d.replying[r.ID]
	d.mu.Unlock()
	if busy {
		return
	}
	end, idle := d.turnEnd(r.ID)
	if known && (!idle || !end.After(sent)) {
		return
	}
	if !known && !d.agentIdle(r.ID) {
		return
	}
	d.mu.Lock()
	if d.replying[r.ID] {
		d.mu.Unlock()
		return
	}
	d.replying[r.ID] = true
	delete(d.fixing, r.ID)
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.replying, r.ID)
			d.mu.Unlock()
		}()
		d.collectReply(r.ID, known)
	}()
}

// collectReply reads the agent's report on the last round's remarks. With none: asked once to write it (when
// asking is for its turn); after that the round keeps what the agent said, with no answers.
func (d *Dispatcher) collectReply(id string, ask bool) {
	r, ok := d.Store.Get(id)
	if !ok || !awaitsReply(r) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	last := lastReview(r)
	ids := make([]string, len(last.Remarks))
	for i, m := range last.Remarks {
		ids[i] = m.ID
	}
	words := d.lastWords(ctx, r)
	report := writer.ParseReport(words, ids)
	if len(report) == 0 {
		if !ask { // after a restart, no report yet: the agent's next turn has it, or is asked for it
			d.mu.Lock()
			d.fixing[id] = time.Now()
			d.mu.Unlock()
			return
		}
		if !last.Asked {
			d.Store.Update(id, func(r *core.Record) {
				if awaitsReply(*r) {
					r.AgentReviews = withLast(r.AgentReviews, func(a *core.AgentReview) { a.Asked = true })
				}
			})
			d.mu.Lock()
			d.fixing[id] = time.Now()
			d.mu.Unlock()
			if err := d.tellAgent(id, i18n.TA("agent.reviewReportAgain", strings.Join(ids, ", "))); err != nil {
				d.mu.Lock()
				delete(d.fixing, id)
				d.mu.Unlock()
				d.Fault(id, "the agent was not asked for a report on the comments", err)
				return
			}
			d.Log(id, "the agent did not write a report on the comments: asked to finish it")
			return
		}
		d.Log(id, "the agent still did not write a report on the comments")
	}
	fixed := false
	for _, a := range report {
		fixed = fixed || a.Reply == core.Fixed || a.Reply == core.Partly
	}
	unverified := fixed && last.SentHead != "" && !landed(ctx, r.Worktree, branchOf(r), last.SentHead, d.pushes(r))
	d.Store.Update(id, func(r *core.Record) {
		if !awaitsReply(*r) {
			return
		}
		r.AgentReviews = withLast(r.AgentReviews, func(a *core.AgentReview) {
			a.Reply, a.ReplyAt, a.Unverified = clipTo(strings.TrimSpace(words), 6000), now(), unverified
			for i := range a.Remarks {
				if x, ok := report[a.Remarks[i].ID]; ok {
					a.Remarks[i].Reply, a.Remarks[i].ReplyNote = x.Reply, x.Note
				}
			}
		})
	})
	if len(report) > 0 {
		d.Log(id, "the agent replied to the reviewer's comments")
	}
	if unverified {
		d.Log(id, "the agent says \"fixed\", but the branch has no new commits since the comments")
	}
	d.publishReply(ctx, id)
}

// landed: the branch has a new commit since sent, and with pushed the remote branch has it too.
func landed(ctx context.Context, wt, branch, sent string, pushed bool) bool {
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); err != nil || head == "" || head == sent {
		return false
	}
	if !pushed {
		return true
	}
	remote, err := gitx.Run(ctx, wt, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	return err == nil && strings.TrimSpace(remote) == head
}

// lastWords: the agent's last message as it is now: its chat's, else its transcript's; the board's view of it
// (agentWords) may be from before the turn for a task whose MR is open.
func (d *Dispatcher) lastWords(ctx context.Context, r core.Record) string {
	if s := d.Chats.Get(r.ID); s != nil {
		if text := s.LastText(); text != "" {
			return text
		}
	}
	if d.LastMessage != nil && r.Worktree != "" {
		if text := d.LastMessage(r.Worktree); text != "" {
			return text
		}
	}
	return d.agentWords(ctx, r)
}

// clipTo: s cut to n bytes at a line or rune boundary, with «…».
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "\n…"
}

// --- comments in the tracker

// reviewTargets: where the settings put the reviewer's comments: the issue, the MR, both; none when off. «mr»
// with no MR yet goes to the issue.
func (d *Dispatcher) reviewTargets(r core.Record) (issue, mr bool) {
	switch d.Config().Reviewer.Comments {
	case "issue":
		return true, false
	case "mr":
		return r.PRURL == "", r.PRURL != ""
	case "both":
		return true, r.PRURL != ""
	}
	return false, false
}

// sayReview leaves text in the task's issue (and, with mr, in its MR), and answers the links to the comments.
func (d *Dispatcher) sayReview(ctx context.Context, r core.Record, text string, issue, mr bool) (issueURL, mrURL string) {
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return "", ""
	}
	if issue {
		var err error
		if c, ok := t.(trackers.Commenter); ok {
			issueURL, err = c.CommentLink(ctx, r.Task, text)
		} else {
			err = t.Comment(ctx, r.Task, text)
		}
		if err != nil {
			d.Fault(r.ID, "the review comment was not added to the task", err)
		}
	}
	if mr && r.PRURL != "" {
		repo, err := d.route(t, r.Task)
		var host trackers.CodeHost
		if err == nil {
			host, _, err = d.host(ctx, t, repo)
		}
		if err == nil {
			if c, ok := host.(trackers.ReviewCommenter); ok {
				mrURL, err = c.CommentReview(ctx, r.PRURL, text)
			} else {
				err = errors.New("the repository code host cannot comment on MRs")
			}
		}
		if err != nil {
			d.Fault(r.ID, "the review comment was not added to the MR", err)
		}
	}
	return issueURL, mrURL
}

// publishReview leaves the task's last round in the tracker, and with the rounds run out, the comment for a
// person.
func (d *Dispatcher) publishReview(ctx context.Context, id string, round int) {
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	issue, mr := d.reviewTargets(r)
	last := lastReview(r)
	if !issue && !mr || !last.Structured() && last.State != "clean" {
		return
	}
	text := writer.ReviewComment(round, last)
	if last.Escalated {
		var open []core.Remark
		for _, m := range core.OpenRemarks(r.AgentReviews) {
			if m.Weight == core.Must {
				open = append(open, m)
			}
		}
		text += "\n\n" + i18n.TA("comment.reviewHuman", round)
		if len(open) > 0 {
			text += "\n\n" + writer.RemarksList(open)
		}
	}
	issueURL, mrURL := d.sayReview(ctx, r, text, issue, mr)
	d.Store.Update(id, func(r *core.Record) {
		if n := len(r.AgentReviews); n > 0 && r.AgentReviews[n-1].Commit == last.Commit {
			r.AgentReviews = withLast(r.AgentReviews, func(a *core.AgentReview) { a.CommentURL, a.MRCommentURL = issueURL, mrURL })
		}
	})
}

// publishReply leaves the agent's report on the last round in the tracker, after the reviewer's comment.
func (d *Dispatcher) publishReply(ctx context.Context, id string) {
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	issue, mr := d.reviewTargets(r)
	if !issue && !mr {
		return
	}
	// Each after the reviewer's comment in the same place, linked to it.
	last := lastReview(r)
	var issueURL, mrURL string
	if issue {
		issueURL, _ = d.sayReview(ctx, r, writer.ReplyComment(last, last.CommentURL), true, false)
	}
	if mr {
		_, mrURL = d.sayReview(ctx, r, writer.ReplyComment(last, last.MRCommentURL), false, true)
	}
	d.Store.Update(id, func(r *core.Record) {
		if n := len(r.AgentReviews); n > 0 && r.AgentReviews[n-1].Commit == last.Commit {
			r.AgentReviews = withLast(r.AgentReviews, func(a *core.AgentReview) { a.ReplyURL, a.MRReplyURL = issueURL, mrURL })
		}
	})
}
