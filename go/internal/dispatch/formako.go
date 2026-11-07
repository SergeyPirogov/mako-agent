package dispatch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// «For Mako»: what Mako already knows it should do better — its own errors that repeat, tasks that failed for the
// same reason, a cost advice that holds for weeks — offered on the board as issues to file. An issue is filed only by
// a person's click, assigned to no one and without the trigger label, so Mako never takes it on its own; one
// suggestion gets one issue, and new facts go to it as a comment while it is open. «No» never offers it again.

// Idea is one suggestion. Title and Detail are Mako's data (an error, a failure's reason, an advice), not words of
// the interface.
type Idea struct {
	// Key: stable across the polls: "fault:…", "fail:…", "cost:…".
	Key    string `json:"key"`
	Source string `json:"source"` // "fault", "fail", "cost"
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Count: the times the error happened, the tasks that failed; 0 for an advice.
	Count int       `json:"count"`
	First time.Time `json:"first,omitzero"`
	Last  time.Time `json:"last,omitzero"`
	Tasks []string  `json:"tasks,omitempty"`
	// Cost: USD spent on the failed tasks, or what the advice would have saved over the last 30 days.
	Cost float64 `json:"cost,omitempty"`
	// Target: where the issue goes by default, "mako" (Mako's repository) or "project" (the tasks' own, for failures
	// that all come from one project other than Mako); Mako: Mako's project as the board names it; Project, Tracker:
	// the tasks' project, when there is one to file into.
	Target  string `json:"target"`
	Mako    string `json:"mako"`
	Project string `json:"project,omitempty"`
	Tracker string `json:"tracker,omitempty"`
	// Issue: the issue filed for it before; filing again adds the new facts to it while it is open.
	Issue string `json:"issue,omitempty"`
}

const (
	ideaRepeats   = 2                   // an error, a failure's reason: from this many times
	ideaFailDays  = 90                  // the failures looked at
	ideaCostDays  = 30                  // an advice's period
	ideaCostHeld  = 14                  // an advice counts when it was given this many days ago as well
	ideaAgain     = 30 * 24 * time.Hour // a filed suggestion still there after this is offered again
	ideasCacheFor = time.Minute         // the board asks every few seconds
	ideaMaxTasks  = 20                  // the tasks named in an issue
	ideaTitleLen  = 90                  // an issue's title, the data part
	ideaFile      = "ideas.json"        // in the state folder
)

// ideaMark: what was done with a suggestion, in ideas.json by its key.
type ideaMark struct {
	No    bool       `json:"no,omitempty"`
	Issue *ideaIssue `json:"issue,omitempty"`
	// Count, At: the suggestion's count when it was filed or commented on last, and when.
	Count int       `json:"count,omitempty"`
	At    time.Time `json:"at,omitzero"`
}

// ideaIssue: the issue a suggestion was filed as.
type ideaIssue struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Tracker string `json:"tracker"`
	URL     string `json:"url,omitempty"`
}

func (is ideaIssue) task() core.Task {
	return core.Task{ID: is.ID, Project: is.Project, TrackerID: is.Tracker, URL: is.URL}
}

type ideasCache struct {
	at   time.Time
	list []Idea
}

func ideaHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

// faultIdeas: Mako's errors that repeat and were not sent for analysis yet.
func faultIdeas(faults []Fault) []Idea {
	out := []Idea{}
	for _, f := range faults {
		if f.Reported != "" || f.Count < ideaRepeats {
			continue
		}
		i := Idea{Key: "fault:" + ideaHash(f.Key), Source: "fault", Title: f.What, Detail: f.Error, Count: f.Count, First: f.First, Last: f.Last, Target: "mako"}
		if f.Task != "" {
			i.Tasks = []string{f.Task}
		}
		out = append(out, i)
	}
	return out
}

// humanEnd: a task that ended by a person's decision (cancelled, closed or deleted in the tracker, written in
// Russian): nothing in Mako to fix.
func humanEnd(r core.Record) bool {
	switch core.ErrorKindOf(r) {
	case core.KindCancelled, core.KindTrackerClosed, core.KindTrackerGone, core.KindRussian:
		return true
	}
	return r.Status == core.Cancelled
}

// endedAt: when the record ended, else when it last changed.
func endedAt(r core.Record) time.Time {
	for _, s := range []string{r.FinishedAt, r.UpdatedAt} {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// failIdeas: the tasks in «Not done» over the last 90 days that failed for the same reason, by that reason. When
// all of them come from one project other than Mako's (makoProject) on a tracker that can open issues (creates),
// the project is the place to file it.
func failIdeas(records []core.Record, now time.Time, makoProject string, creates func(tracker string) bool) []Idea {
	type group struct {
		text string
		rs   []core.Record
	}
	groups := map[string]*group{}
	since := now.AddDate(0, 0, -ideaFailDays)
	for _, r := range records {
		failed := r.Status == core.Failed || r.ReviewState == "closed"
		if !failed || humanEnd(r) || endedAt(r).Before(since) {
			continue
		}
		key, text := core.ReasonOf(r)
		if groups[key] == nil {
			groups[key] = &group{text: text}
		}
		groups[key].rs = append(groups[key].rs, r)
	}
	out := []Idea{}
	for key, g := range groups {
		if len(g.rs) < ideaRepeats {
			continue
		}
		sort.Slice(g.rs, func(a, b int) bool { return endedAt(g.rs[a]).After(endedAt(g.rs[b])) })
		i := Idea{Key: "fail:" + ideaHash(key), Source: "fail", Title: g.text, Detail: clipText(strings.TrimSpace(g.rs[0].Error), maxFaultText),
			Count: len(g.rs), First: endedAt(g.rs[len(g.rs)-1]), Last: endedAt(g.rs[0]), Target: "mako"}
		projects, trackerIDs := map[string]bool{}, map[string]bool{}
		for _, r := range g.rs {
			i.Cost += r.Spent()
			if len(i.Tasks) < ideaMaxTasks {
				i.Tasks = append(i.Tasks, r.ID)
			}
			projects[r.Task.Project], trackerIDs[r.Task.TrackerID] = true, true
		}
		if p, tr := g.rs[0].Task.Project, g.rs[0].Task.TrackerID; len(projects) == 1 && len(trackerIDs) == 1 && p != "" &&
			!strings.EqualFold(p, makoProject) && creates(tr) {
			i.Target, i.Project, i.Tracker = "project", p, tr
		}
		out = append(out, i)
	}
	return out
}

// costIdeas: the advice on what to spend less on (not one waiting for tasks) that was given for the last 30 days
// (now) and for the 30 days that ended two weeks ago (old) as well: it holds, it is not a week's chance.
func costIdeas(now, old []core.Advice) []Idea {
	key := func(a core.Advice) string {
		if a.Phase == "" && a.Size == "" {
			return "cost:" + a.Kind
		}
		return "cost:" + a.Phase + "|" + a.Size
	}
	held := map[string]bool{}
	for _, a := range old {
		if a.Kind != "wait" {
			held[key(a)] = true
		}
	}
	out := []Idea{}
	for _, a := range now {
		if a.Kind == "wait" || !held[key(a)] {
			continue
		}
		detail := a.Text
		for _, row := range a.Rows {
			detail += "\n" + row.Label + ": " + row.Figures
		}
		out = append(out, Idea{Key: key(a), Source: "cost", Title: a.Title, Detail: strings.TrimSpace(detail), Cost: a.Save, Target: "mako"})
	}
	return out
}

// takenBetween: the records handed off (or, never handed off, last changed) in [from, to).
func takenBetween(records []core.Record, from, to time.Time) []core.Record {
	out := []core.Record{}
	for _, r := range records {
		at := r.StartedAt
		if at == "" {
			at = r.UpdatedAt
		}
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil && !t.Before(from) && t.Before(to) {
			out = append(out, r)
		}
	}
	return out
}

// makoProject: the host and project of Mako's own repository — the origin of the clone it is built from, else
// MakoRepo.
func (d *Dispatcher) makoProject(ctx context.Context) (host, project string) {
	host, project = "github.com", strings.ToLower(strings.TrimPrefix(MakoRepo, "github.com/"))
	if d.Source != "" {
		if origin, _ := gitx.OriginURL(ctx, d.Source); gitx.RemoteProject(origin) != "" {
			host, project = remoteHost(origin), gitx.RemoteProject(origin)
		}
	}
	return host, project
}

// allIdeas: every suggestion there is now, before the marks of ideas.json.
func (d *Dispatcher) allIdeas(ctx context.Context, now time.Time) []Idea {
	cfg := d.Config()
	records := d.Store.All()
	_, mako := d.makoProject(ctx)
	// Mako's project as the board names it, when its tasks are there.
	for _, r := range records {
		if strings.EqualFold(r.Task.Project, mako) {
			mako = r.Task.Project
			break
		}
	}
	creates := func(id string) bool {
		_, ok := d.tracker(id).(trackers.Creator)
		return ok
	}
	out := faultIdeas(d.Faults())
	out = append(out, failIdeas(records, now, mako, creates)...)
	routes := d.RoutesNow()
	recent := core.CostAdvice(core.SummarizePriced(records, now, now.AddDate(0, 0, -ideaCostDays), cfg.Prices), routes, cfg.RouteMode)
	before := now.AddDate(0, 0, -ideaCostHeld)
	oldRecords := takenBetween(records, now.AddDate(0, 0, -ideaCostDays-ideaCostHeld), before)
	old := core.CostAdvice(core.SummarizePriced(oldRecords, before, now.AddDate(0, 0, -ideaCostDays-ideaCostHeld), cfg.Prices), routes, cfg.RouteMode)
	out = append(out, costIdeas(recent, old)...)
	for i := range out {
		out[i].Mako = mako
	}
	return out
}

// Ideas: the suggestions to show: without the declined ones and without the filed ones, unless they grew since
// or still hold a month later.
func (d *Dispatcher) Ideas() []Idea {
	now := time.Now()
	d.ideasMu.Lock()
	if c := d.ideasSeen; c != nil && now.Sub(c.at) < ideasCacheFor {
		d.ideasMu.Unlock()
		return c.list
	}
	d.ideasMu.Unlock()
	all := d.allIdeas(context.Background(), now)
	d.ideasMu.Lock()
	defer d.ideasMu.Unlock()
	marks := d.readIdeas()
	list := []Idea{}
	for _, i := range all {
		m := marks[i.Key]
		if m.No {
			continue
		}
		if m.Issue != nil {
			if i.Count <= m.Count && now.Sub(m.At) < ideaAgain {
				continue
			}
			i.Issue = m.Issue.URL
			if i.Issue == "" {
				i.Issue = m.Issue.ID
			}
		}
		list = append(list, i)
	}
	rank := map[string]int{"fault": 0, "fail": 1, "cost": 2}
	sort.SliceStable(list, func(a, b int) bool {
		if rank[list[a].Source] != rank[list[b].Source] {
			return rank[list[a].Source] < rank[list[b].Source]
		}
		return list[a].Count > list[b].Count
	})
	d.ideasSeen = &ideasCache{at: now, list: list}
	return list
}

// FileIdea files the suggestion as an issue in Mako's repository (target "mako") or the tasks' project ("project"),
// with no assignee and no trigger label; when one filed before there is still open, the new facts go to it as a
// comment instead (commented). It returns the issue's link.
func (d *Dispatcher) FileIdea(ctx context.Context, key, target string) (url string, commented bool, err error) {
	d.ideaFiling.Lock() // a second click while the first is filing must not open a second issue
	defer d.ideaFiling.Unlock()
	var idea *Idea
	for _, i := range d.allIdeas(ctx, time.Now()) {
		if i.Key == key {
			idea = &i
			break
		}
	}
	if idea == nil {
		return "", false, fmt.Errorf("this suggestion is gone: refresh the board")
	}
	var trackerID, project string
	if target == "project" && idea.Project != "" {
		trackerID, project = idea.Tracker, idea.Project
	} else if trackerID, project, err = d.faultTarget(ctx); err != nil {
		return "", false, err
	}
	t, c, err := d.creator(trackerID)
	if err != nil {
		return "", false, err
	}
	d.ideasMu.Lock()
	mark := d.readIdeas()[key]
	d.ideasMu.Unlock()
	if mark.No {
		return "", false, fmt.Errorf("this suggestion was declined")
	}
	var filed ideaIssue
	if is := mark.Issue; is != nil && is.Tracker == t.ID() && strings.EqualFold(is.Project, project) {
		// Open, or the tracker cannot tell: the facts go to it; closed or deleted: a new issue.
		if st, err := t.IssueState(ctx, is.task()); err != nil || st == "open" {
			if err := t.Comment(ctx, is.task(), ideaComment(*idea)); err != nil {
				return "", false, err
			}
			filed, commented = *is, true
		}
	}
	if !commented {
		task, err := c.Create(ctx, trackers.NewIssue{Project: project, Title: ideaTitle(*idea), Description: ideaBody(*idea, d.Version), Plain: true})
		if err != nil {
			return "", false, err
		}
		filed = ideaIssue{ID: task.ID, Project: task.Project, Tracker: t.ID(), URL: task.URL}
	}
	link := filed.URL
	if link == "" {
		link = filed.ID
	}
	d.ideasMu.Lock()
	marks := d.readIdeas()
	marks[key] = ideaMark{Issue: &filed, Count: idea.Count, At: time.Now()}
	err = d.writeIdeas(marks)
	d.ideasSeen = nil
	d.ideasMu.Unlock()
	if err != nil {
		log.Printf("%s: not saved: %v", ideaFile, err)
	}
	if commented {
		d.Log("mako", "For Mako: new facts added to "+link+": "+idea.Title)
	} else {
		d.Log("mako", "For Mako: issue filed "+link+": "+idea.Title)
	}
	return link, commented, nil
}

// DeclineIdea: «No» — the suggestion is never offered again.
func (d *Dispatcher) DeclineIdea(key string) error {
	if !strings.HasPrefix(key, "fault:") && !strings.HasPrefix(key, "fail:") && !strings.HasPrefix(key, "cost:") {
		return fmt.Errorf("unknown suggestion %q", key)
	}
	d.ideasMu.Lock()
	defer d.ideasMu.Unlock()
	marks := d.readIdeas()
	m := marks[key]
	m.No, m.At = true, time.Now()
	marks[key] = m
	d.ideasSeen = nil
	return d.writeIdeas(marks)
}

// --- the issue's words: in the agents' language; what Mako quotes is data, kept apart in a code block.

func ideaTitle(i Idea) string {
	what := strings.Join(strings.Fields(i.Title), " ")
	if r := []rune(what); len(r) > ideaTitleLen {
		what = string(r[:ideaTitleLen]) + "…"
	}
	return i18n.TA("idea.title."+i.Source, what)
}

// ideaFacts: the facts, a Markdown list.
func ideaFacts(i Idea) string {
	var b strings.Builder
	if i.Source == "fault" {
		fmt.Fprintln(&b, i18n.TA("idea.fact.times", i.Count))
	}
	if i.Source == "fail" {
		fmt.Fprintln(&b, i18n.TA("idea.fact.failed", i.Count))
	}
	if !i.First.IsZero() {
		fmt.Fprintln(&b, i18n.TA("idea.fact.period", i.First.Format("02.01.2006 15:04"), i.Last.Format("02.01.2006 15:04")))
	}
	if len(i.Tasks) > 0 {
		fmt.Fprintln(&b, i18n.TA("idea.fact.tasks", "`"+strings.Join(i.Tasks, "`, `")+"`"))
	}
	if i.Cost > 0 && i.Source == "fail" {
		fmt.Fprintln(&b, i18n.TA("idea.fact.spent", i.Cost))
	}
	if i.Cost > 0 && i.Source == "cost" {
		fmt.Fprintln(&b, i18n.TA("idea.fact.save", ideaCostDays, i.Cost))
	}
	return b.String()
}

// ideaQuote: Mako's data as a code block, so its words are read as data.
func ideaQuote(i Idea) string {
	text := strings.TrimSpace(i.Title)
	if i.Detail != "" && i.Detail != i.Title {
		text += "\n\n" + strings.TrimSpace(i.Detail)
	}
	return i18n.TA("idea.data") + "\n\n```\n" + strings.ReplaceAll(text, "```", "'''") + "\n```\n"
}

func ideaBody(i Idea, version string) string {
	if version == "" {
		version = "dev"
	}
	var b strings.Builder
	b.WriteString(i18n.TA("idea.intro."+i.Source) + "\n\n")
	b.WriteString(ideaFacts(i) + "\n")
	b.WriteString(ideaQuote(i) + "\n")
	fmt.Fprintf(&b, "Mako `%s`, %s/%s\n\n", version, runtime.GOOS, runtime.GOARCH)
	b.WriteString(i18n.TA("idea.footer") + "\n")
	return b.String()
}

func ideaComment(i Idea) string {
	return i18n.TA("idea.comment") + "\n\n" + ideaFacts(i) + "\n" + ideaQuote(i)
}

// --- ideas.json

func (d *Dispatcher) ideasFile() string {
	if d.Config().StateDir == "" {
		return ""
	}
	return filepath.Join(d.Config().StateDir, ideaFile)
}

// readIdeas: the marks by key; none when the file is missing or unreadable (an older Mako had none). Under ideasMu.
func (d *Dispatcher) readIdeas() map[string]ideaMark {
	marks := map[string]ideaMark{}
	if d.ideasFile() == "" {
		return marks
	}
	if b, err := os.ReadFile(d.ideasFile()); err == nil {
		if err := json.Unmarshal(b, &marks); err != nil {
			log.Printf("%s: not read: %v", ideaFile, err)
			return map[string]ideaMark{}
		}
	}
	return marks
}

func (d *Dispatcher) writeIdeas(marks map[string]ideaMark) error {
	if d.ideasFile() == "" {
		return nil
	}
	b, _ := json.MarshalIndent(marks, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.ideasFile()), 0o755); err != nil {
		return err
	}
	return fsx.WriteFile(d.ideasFile(), b, 0o600)
}
