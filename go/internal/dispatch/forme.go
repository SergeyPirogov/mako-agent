package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// «Підготувати ревʼю»: the reviewer agent reads someone else's MR for me (in a read-only worktree at its head
// when the project's clone is on this Mac, else from its diff only) and drafts my review: what it changes and
// why, its risks as comments, what to check by hand, whether to merge it. I edit the comments, strike some out
// and send the rest to the MR from me; «Схвалити» approves it.

// ReviewForMe runs the reviewer over someone else's MR; tests replace it.
var ReviewForMe = writer.ForeignReview

// forMeSpendFile: where the reviewer's spend over others' MRs is kept, in the state's directory.
const forMeSpendFile = "review-for-me-spend.json"

// PrepareReview starts the reviewer over the MR to review; its draft replaces the one the card had.
func (d *Dispatcher) PrepareReview(id string) error {
	rq, inbox, err := d.forMe(id)
	if err != nil {
		return err
	}
	if rq.State != "open" {
		return errors.New("the MR is done: there is nothing to review")
	}
	d.mu.Lock()
	if d.preparing[id] {
		d.mu.Unlock()
		return errors.New("the reviewer is already reading this MR")
	}
	d.preparing[id] = true
	d.mu.Unlock()
	// What the earlier draft has stays on the card meanwhile: a failed run leaves it as it was.
	d.setDraft(id, func(dr *core.ReviewDraft) { dr.State, dr.Error, dr.At = "running", "", now() })
	d.Log(id, "reviewer is preparing your review of the MR")
	go d.prepare(id, inbox)
	return nil
}

// forMe: the MR to review and its code host.
func (d *Dispatcher) forMe(id string) (core.ReviewRequest, trackers.ReviewInbox, error) {
	if d.Reviews == nil {
		return core.ReviewRequest{}, nil, fmt.Errorf("no such MR to review: %s", id)
	}
	rq, ok := d.Reviews.Get(id)
	if !ok {
		return rq, nil, fmt.Errorf("no such MR to review: %s", id)
	}
	t := d.tracker(rq.TrackerID)
	inbox, ok := t.(trackers.ReviewInbox)
	if !ok || !t.Config().ReviewRequests {
		return rq, nil, fmt.Errorf("tracker %s cannot show MRs", rq.TrackerID)
	}
	return rq, inbox, nil
}

func (d *Dispatcher) prepare(id string, inbox trackers.ReviewInbox) {
	defer func() {
		d.mu.Lock()
		delete(d.preparing, id)
		d.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	rq, ok := d.Reviews.Get(id)
	if !ok {
		return
	}
	var cost float64
	var tokens int
	fail := func(err error) {
		d.setDraft(id, func(dr *core.ReviewDraft) {
			dr.State, dr.Error = "failed", err.Error()
			dr.Cost, dr.Tokens = cost, tokens
		})
		d.Log(id, "reviewer failed: "+err.Error())
	}
	files, err := inbox.ReviewFiles(ctx, rq.URL)
	if err != nil {
		fail(err)
		return
	}
	dir, limited, done, err := d.reviewDir(ctx, rq)
	if err != nil {
		fail(err)
		return
	}
	defer done()
	route := d.forMeRoute(rq)
	sctx := writer.WithSpend(ctx, func(s writer.Spend) {
		cost += s.USD
		tokens += s.Usage.Total()
		if d.Spends != nil {
			d.Spends.Add(core.Spend{At: now(), Phase: core.PhaseForMe, Project: rq.Project, USD: s.USD, Usage: s.Usage})
		}
	})
	mr := writer.ForeignMR{Ref: rq.Ref, Title: rq.Title, Description: rq.Description, Author: rq.Author, Diff: filesDiff(files), Limited: limited}
	text, err := ReviewForMe(sctx, dir, mr, route)
	if err != nil {
		fail(err)
		return
	}
	draft := writer.ParseForeignReview(text)
	draft.State, draft.At, draft.Commit, draft.Limited = "ready", now(), rq.HeadSHA, limited
	draft.Cost, draft.Tokens = cost, tokens
	if _, ok := d.Reviews.Get(id); !ok {
		return // the MR left the list meanwhile
	}
	d.Reviews.Update(id, func(r *core.ReviewRequest) {
		draft.Comments = carrySent(r.Draft, draft.Comments)
		r.Draft = &draft
	})
	d.Log(id, fmt.Sprintf("reviewer prepared your review (%s): %d comments to send", route.Label(), len(draft.Comments)))
	notify.Inform(notify.AgentReview, i18n.T("notify.reviewPrepared", rq.Ref), rq.Title, d.reviewLink(id))
}

// carrySent: the new draft's comments after the ones of the earlier draft already sent to the MR, so the card
// still tells what went and nothing is sent twice. The new ones are numbered after every earlier one: a send
// that answers after the new draft came marks its own comments, by ID, never new ones.
func carrySent(old *core.ReviewDraft, fresh []core.DraftComment) []core.DraftComment {
	var out []core.DraftComment
	next := 1
	if old != nil {
		for _, c := range old.Comments {
			next = max(next, core.RemarkNum(c.ID)+1)
			if c.State == core.DraftSent {
				out = append(out, c)
			}
		}
	}
	for i, c := range fresh {
		c.ID = "R" + strconv.Itoa(next+i)
		out = append(out, c)
	}
	return out
}

// setDraft changes the MR's draft (a copy: the store hands out shallow ones) under the store's lock.
func (d *Dispatcher) setDraft(id string, change func(dr *core.ReviewDraft)) core.ReviewRequest {
	return d.Reviews.Update(id, func(r *core.ReviewRequest) {
		dr := r.Draft.Clone()
		change(dr)
		r.Draft = dr
	})
}

// interruptedDrafts: a draft the reviewer was writing when Mako stopped will not come: it failed.
func (d *Dispatcher) interruptedDrafts() {
	for _, r := range d.Reviews.All() {
		if r.Draft != nil && r.Draft.State == "running" {
			d.setDraft(r.ID, func(dr *core.ReviewDraft) {
				dr.State, dr.Error = "failed", "Mako was restarted while the reviewer was reading the MR"
			})
		}
	}
}

// filesDiff: the MR's files as one diff, for the reviewer.
func filesDiff(files []trackers.ReviewFile) string {
	var b strings.Builder
	for _, f := range files {
		old, path := f.Path, f.Path
		if f.OldPath != "" {
			old = f.OldPath
		}
		from, to := "a/"+old, "b/"+path
		switch f.Status {
		case "A":
			from = "/dev/null"
		case "D":
			to = "/dev/null"
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- %s\n+++ %s\n", old, path, from, to)
		if f.Diff == "" {
			b.WriteString("(the diff is not shown: the file is too large or binary)\n")
			continue
		}
		b.WriteString(strings.TrimRight(f.Diff, "\n") + "\n")
	}
	return b.String()
}

// forMeSize: an MR's size for the reviewer's route, by the lines it changes.
func forMeSize(lines int) string {
	switch {
	case lines < 100:
		return "small"
	case lines < 500:
		return "medium"
	}
	return "big"
}

// forMeRoute: the reviewer's route over someone else's MR: the review phase of the table, for the MR's size,
// with the mode of the project's repository.
func (d *Dispatcher) forMeRoute(rq core.ReviewRequest) core.Route {
	mode := ""
	if p, ok := d.forMeClone(rq); ok && p.repo != nil {
		mode = p.repo.RouteMode
	}
	lines := rq.Additions + rq.Deletions
	size := forMeSize(lines)
	return routeAt(d.Config(), core.Record{}, core.PhaseReview, mode, size, fmt.Sprintf("%s (%d lines)", size, lines))
}

// forMeClone: the clone of the MR's project on this Mac.
func (d *Dispatcher) forMeClone(rq core.ReviewRequest) (CodeProject, bool) {
	for _, p := range d.CodeProjects() {
		if p.Tracker == rq.TrackerID && p.Project == rq.Project {
			return p, true
		}
	}
	return CodeProject{}, false
}

// reviewDir: where the reviewer reads the MR: a worktree of the project's clone at the MR's head, else an empty
// directory (limited: the diff is all it has). done takes it away.
func (d *Dispatcher) reviewDir(ctx context.Context, rq core.ReviewRequest) (dir string, limited bool, done func(), err error) {
	if p, ok := d.forMeClone(rq); ok {
		dir, err := mrWorktree(ctx, p, rq)
		if err == nil {
			return dir, false, func() { dropMRWorktree(p, dir) }, nil
		}
		log.Printf("[%s] no worktree of the MR, the reviewer reads its diff only: %v", rq.ID, err)
	}
	tmp, err := os.MkdirTemp("", "mako-review-")
	if err != nil {
		return "", true, nil, err
	}
	return tmp, true, func() { os.RemoveAll(tmp) }, nil
}

// mrRef: the ref the code host keeps the MR's head under.
func mrRef(rq core.ReviewRequest) string {
	if strings.HasPrefix(rq.Ref, "#") {
		return "refs/pull/" + strconv.Itoa(rq.Number) + "/head"
	}
	return "refs/merge-requests/" + strconv.Itoa(rq.Number) + "/head"
}

// mrWorktree: a worktree of the clone at the MR's head (a detached HEAD), next to the shared one of «Запитати
// про код»; the head is fetched from the code host's ref of the MR, a fork's commits too.
func mrWorktree(ctx context.Context, p CodeProject, rq core.ReviewRequest) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := gitx.Run(fctx, p.Path, "fetch", "--quiet", "origin", mrRef(rq)); err != nil {
		return "", err
	}
	head := rq.HeadSHA
	if head == "" {
		head = "FETCH_HEAD"
	}
	if _, err := gitx.Run(ctx, p.Path, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
		return "", fmt.Errorf("the MR's head %s is not in the clone", head)
	}
	dir := filepath.Join(filepath.Dir(askDir(p)), "mako-review-"+strconv.Itoa(rq.Number))
	if exists(dir) { // left by a run Mako did not finish
		dropMRWorktree(p, dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	gitx.Run(ctx, p.Path, "worktree", "prune")
	if _, err := gitx.Run(ctx, p.Path, "worktree", "add", "--detach", dir, head); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func dropMRWorktree(p CodeProject, dir string) {
	if err := gitx.DropWorktree(context.Background(), p.Path, dir, ""); err != nil {
		os.RemoveAll(dir)
		gitx.Run(context.Background(), p.Path, "worktree", "prune")
	}
}

// EditDraft: a comment of the draft changed by me: its text, or struck out (dropped) and back.
func (d *Dispatcher) EditDraft(id, comment, text string, dropped bool) error {
	rq, _, err := d.forMe(id)
	if err != nil {
		return err
	}
	if rq.Draft == nil {
		return errors.New("the MR has no draft review")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the comment is empty")
	}
	// Looked up by its ID under the store's lock: another run may have replaced the draft since it was read.
	var found, sent bool
	d.setDraft(id, func(dr *core.ReviewDraft) {
		i := slices.IndexFunc(dr.Comments, func(c core.DraftComment) bool { return c.ID == comment })
		if i < 0 {
			return
		}
		found = true
		c := &dr.Comments[i]
		if sent = c.State == core.DraftSent; sent {
			return
		}
		c.Text, c.Error = text, ""
		c.State = core.DraftOpen
		if dropped {
			c.State = core.DraftDropped
		}
	})
	switch {
	case !found:
		return fmt.Errorf("no such comment: %s", comment)
	case sent:
		return errors.New("the comment is sent already")
	}
	return nil
}

// SendDrafts posts the draft's comments I chose (ids) to the MR from me; the struck-out and sent ones are not.
// Each says how it went; the error is the review's as a whole.
func (d *Dispatcher) SendDrafts(ctx context.Context, id string, ids []string) (core.ReviewRequest, error) {
	rq, poster, err := d.poster(id)
	if err != nil {
		return rq, err
	}
	if rq.Draft == nil {
		return rq, errors.New("the MR has no draft review")
	}
	var at []string // the comments posted, by ID: the draft may be another one by the time the host answers
	var notes []trackers.DraftNote
	for _, c := range rq.Draft.Comments {
		if !slices.Contains(ids, c.ID) || c.State == core.DraftDropped || c.State == core.DraftSent {
			continue
		}
		at = append(at, c.ID)
		notes = append(notes, trackers.DraftNote{Path: c.File, Line: c.Line, Body: c.Text})
	}
	if len(notes) == 0 {
		return rq, errors.New("no comments to send")
	}
	res, err := poster.PostReview(ctx, rq.URL, rq.Draft.Commit, notes)
	if err != nil {
		return rq, err
	}
	sent := 0
	rq = d.setDraft(id, func(dr *core.ReviewDraft) {
		for k, cid := range at {
			i := slices.IndexFunc(dr.Comments, func(c core.DraftComment) bool { return c.ID == cid })
			if i < 0 || k >= len(res) {
				continue
			}
			c := &dr.Comments[i]
			if res[k].Error != "" {
				c.State, c.Error = core.DraftFailed, res[k].Error
				continue
			}
			c.State, c.URL, c.Error = core.DraftSent, res[k].URL, ""
			sent++
		}
	})
	d.Log(id, fmt.Sprintf("%d of %d comments sent to the MR", sent, len(notes)))
	return rq, nil
}

// ApproveReview approves the MR from me: it goes to «Готово».
func (d *Dispatcher) ApproveReview(ctx context.Context, id string) error {
	rq, poster, err := d.poster(id)
	if err != nil {
		return err
	}
	if rq.State != "open" {
		return errors.New("the MR is done already")
	}
	if err := poster.ApproveReview(ctx, rq.URL); err != nil {
		return err
	}
	d.finishReview(id, "approved")
	return nil
}

// poster: the MR to review and its code host, which takes my review.
func (d *Dispatcher) poster(id string) (core.ReviewRequest, trackers.ReviewPoster, error) {
	rq, inbox, err := d.forMe(id)
	if err != nil {
		return rq, nil, err
	}
	poster, ok := inbox.(trackers.ReviewPoster)
	if !ok {
		return rq, nil, fmt.Errorf("tracker %s cannot send a review", rq.TrackerID)
	}
	return rq, poster, nil
}
