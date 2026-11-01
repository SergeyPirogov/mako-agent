package core

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Stats is how the taken tasks went: how many ended well and how many did not, and why, how long a task takes
// and what the agent spent.
type Stats struct {
	Taken     int `json:"taken"`     // given to an agent (or tried to be)
	Succeeded int `json:"succeeded"` // an MR is open or merged
	Merged    int `json:"merged"`
	Failed    int `json:"failed"` // failed, cancelled or its MR closed unmerged
	Running   int `json:"running"`
	// AvgSeconds: from the hand-off to the MR, over the tasks that got one.
	AvgSeconds float64  `json:"avgSeconds"`
	Reasons    []Reason `json:"reasons"` // why tasks failed, the most frequent first
	// Cost: USD Mako's own agent spent (an agent in Orca does not report it); Priced: the tasks it is known for.
	Cost   float64 `json:"cost"`
	Priced int     `json:"priced"`
	// MedianSeconds: the median of the same times, which one very long task does not pull up.
	MedianSeconds float64 `json:"medianSeconds"`
	// ReviewSeconds: on average from the MR to its merge, over the merged tasks.
	ReviewSeconds float64 `json:"reviewSeconds"`
	Retried       int     `json:"retried"`  // taken more than once
	LastWeek      int     `json:"lastWeek"` // taken in the last 7 days
	// Projects: the same counts for each project, the busiest first.
	Projects []Group `json:"projects"`
	// Executors: by who did the work: Mako's own agent or an executor in Orca.
	Executors []Group `json:"executors"`
	// Models: by the model the agent worked with ("—" unknown), the busiest first.
	Models []Group `json:"models"`
	// Assessed: the tasks the agent rated; the rest are the average scores over them.
	Assessed     int     `json:"assessed"`
	Clarity      float64 `json:"clarity"`
	Complexity   float64 `json:"complexity"`
	Completeness float64 `json:"completeness"`
	Risk         float64 `json:"risk"`
	// Forecast: the size the estimate expected before the start against the one the finished task came to.
	Forecast Forecast `json:"forecast"`
	// BySize: how the tasks of each expected size went, in the order of Sizes.
	BySize []SizeGroup `json:"bySize"`
	// Authors: by who wrote the task in the tracker, the busiest first.
	Authors []Group `json:"authors"`
}

// Forecast compares the estimate before the start with the assessment after the MR, over the tasks that have
// both sizes.
type Forecast struct {
	Compared int `json:"compared"`
	Hits     int `json:"hits"` // the same size both times
	// Matrix[expected][came to]: how many tasks, both in the order of Sizes.
	Matrix [][]int `json:"matrix"`
}

// SizeGroup is how the tasks the estimate called one size went.
type SizeGroup struct {
	Size  string `json:"size"`
	Taken int    `json:"taken"`
	// Ended: merged or failed, whose outcome is known; Clean: merged with no rework («Повторити інакше», another
	// attempt, fixes after a review).
	Ended int `json:"ended"`
	Clean int `json:"clean"`
	// Seconds: the agent's average time to the MR over the Timed tasks; Cost: the average USD over the Priced.
	Seconds float64 `json:"seconds"`
	Timed   int     `json:"timed"`
	Cost    float64 `json:"cost"`
	Priced  int     `json:"priced"`
}

// Group is how the tasks of one project or one executor went.
type Group struct {
	Name      string  `json:"name"`
	Taken     int     `json:"taken"`
	Succeeded int     `json:"succeeded"`
	Failed    int     `json:"failed"`
	Cost      float64 `json:"cost"`
	// Clarity: the average score of how well the tasks were described, over the Assessed ones.
	Clarity  float64 `json:"clarity"`
	Assessed int     `json:"assessed"`
	// Unclear: the Assessed ones the agent hardly understood (clarity 1–2).
	Unclear int `json:"unclear"`
}

// unclear: the highest clarity of a task the agent hardly understood.
const unclear = 2

type Reason struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// Summarize counts the records of the tasks that were taken since (zero: all of them); ones never handed off
// are not in it.
func Summarize(records []Record, now, since time.Time) Stats {
	st := Stats{Reasons: []Reason{}, Forecast: Forecast{Matrix: make([][]int, len(Sizes))}, BySize: make([]SizeGroup, len(Sizes))}
	for i, size := range Sizes {
		st.Forecast.Matrix[i] = make([]int, len(Sizes))
		st.BySize[i].Size = size
	}
	reasons := map[string]int{}
	projects, executors, authors, models := map[string]*Group{}, map[string]*Group{}, map[string]*Group{}, map[string]*Group{}
	var took []float64
	var review time.Duration
	reviewed := 0
	weekAgo := now.Add(-7 * 24 * time.Hour)
	for _, r := range records {
		if r.StartedAt == "" && r.Status != Failed {
			continue
		}
		if !since.IsZero() && !takenSince(r, since) {
			continue
		}
		st.Taken++
		if r.Attempt > 1 {
			st.Retried++
		}
		if t, err := time.Parse(time.RFC3339Nano, r.StartedAt); err == nil && t.After(weekAgo) {
			st.LastWeek++
		}
		project := r.Task.Project
		if project == "" {
			project = r.Repo
		}
		executor := r.Executor
		if executor == "" {
			executor = "—"
		}
		model := r.AgentModel
		if model == "" {
			model = "—"
		}
		groups := []*Group{group(projects, project), group(executors, executor), group(models, model)}
		if r.Task.Author != "" {
			groups = append(groups, group(authors, r.Task.Author))
		}
		if r.Cost > 0 {
			st.Cost += r.Cost
			st.Priced++
		}
		for _, g := range groups {
			g.Taken++
			g.Cost += r.Cost
		}
		if a := r.Assessment; a != nil && a.State == "done" {
			st.Assessed++
			st.Clarity += float64(a.Clarity.Value)
			st.Complexity += float64(a.Complexity.Value)
			st.Completeness += float64(a.Completeness.Value)
			st.Risk += float64(a.Risk.Value)
			for _, g := range groups {
				g.Assessed++
				g.Clarity += float64(a.Clarity.Value)
				if a.Clarity.Value <= unclear {
					g.Unclear++
				}
			}
		}
		bySize(&st, r)
		switch {
		case r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed":
			st.Failed++
			reasons[reason(r.Error)]++
			for _, g := range groups {
				g.Failed++
			}
		case r.Status == Done:
			st.Succeeded++
			for _, g := range groups {
				g.Succeeded++
			}
			if r.ReviewState == "merged" {
				st.Merged++
				if d, ok := between(r.FinishedAt, r.MergedAt); ok {
					review += d
					reviewed++
				}
			}
			if d, ok := between(r.StartedAt, r.FinishedAt); ok {
				took = append(took, d.Seconds())
			}
		default:
			st.Running++
		}
	}
	if len(took) > 0 {
		sum := 0.0
		for _, s := range took {
			sum += s
		}
		st.AvgSeconds = sum / float64(len(took))
		sort.Float64s(took)
		if n := len(took); n%2 == 1 {
			st.MedianSeconds = took[n/2]
		} else {
			st.MedianSeconds = (took[n/2-1] + took[n/2]) / 2
		}
	}
	if reviewed > 0 {
		st.ReviewSeconds = review.Seconds() / float64(reviewed)
	}
	if n := float64(st.Assessed); n > 0 {
		st.Clarity, st.Complexity, st.Completeness, st.Risk = st.Clarity/n, st.Complexity/n, st.Completeness/n, st.Risk/n
	}
	for i := range st.BySize {
		g := &st.BySize[i]
		if g.Timed > 0 {
			g.Seconds /= float64(g.Timed)
		}
		if g.Priced > 0 {
			g.Cost /= float64(g.Priced)
		}
	}
	st.Projects, st.Executors, st.Authors, st.Models = sorted(projects), sorted(executors), sorted(authors), sorted(models)
	for text, n := range reasons {
		st.Reasons = append(st.Reasons, Reason{Text: text, Count: n})
	}
	sort.Slice(st.Reasons, func(i, j int) bool {
		a, b := st.Reasons[i], st.Reasons[j]
		return a.Count > b.Count || a.Count == b.Count && a.Text < b.Text
	})
	return st
}

// bySize counts r by the size its estimate expected, and against the size it came to when it has both.
func bySize(st *Stats, r Record) {
	e := r.Estimate
	if e == nil || e.State != "done" {
		return
	}
	want := slices.Index(Sizes, e.Size)
	if want < 0 {
		return
	}
	if a := r.Assessment; a != nil && a.State == "done" {
		if got := slices.Index(Sizes, a.Size); got >= 0 {
			st.Forecast.Compared++
			st.Forecast.Matrix[want][got]++
			if got == want {
				st.Forecast.Hits++
			}
		}
	}
	g := &st.BySize[want]
	g.Taken++
	failed := r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed"
	merged := r.Status == Done && r.ReviewState == "merged"
	if failed || merged {
		g.Ended++
	}
	if merged && r.Attempt <= 1 && r.ReviewFixes == 0 && !reviewed(r) {
		g.Clean++
	}
	if r.Status == Done {
		if d, ok := between(r.StartedAt, r.FinishedAt); ok {
			g.Seconds += d.Seconds()
			g.Timed++
		}
	}
	if r.Cost > 0 {
		g.Cost += r.Cost
		g.Priced++
	}
}

// reviewed: the reviewer agent sent the task's agent remarks to fix.
func reviewed(r Record) bool {
	for _, v := range r.AgentReviews {
		if v.State == "notes" {
			return true
		}
	}
	return false
}

// takenSince: the task was handed off (or, never handed off, last changed) at since or later.
func takenSince(r Record, since time.Time) bool {
	at := r.StartedAt
	if at == "" {
		at = r.UpdatedAt
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	return err == nil && !t.Before(since)
}

func group(groups map[string]*Group, name string) *Group {
	if groups[name] == nil {
		groups[name] = &Group{Name: name}
	}
	return groups[name]
}

func sorted(groups map[string]*Group) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		if g.Assessed > 0 {
			g.Clarity /= float64(g.Assessed)
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Taken > out[j].Taken || out[i].Taken == out[j].Taken && out[i].Name < out[j].Name
	})
	return out
}

// reason: the first line of the error, short, so the same failure counts once.
func reason(err string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(err), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "Без причини"
	}
	if r := []rune(line); len(r) > 160 {
		line = string(r[:160]) + "…"
	}
	return line
}

func between(from, to string) (time.Duration, bool) {
	a, err1 := time.Parse(time.RFC3339Nano, from)
	b, err2 := time.Parse(time.RFC3339Nano, to)
	if err1 != nil || err2 != nil || !b.After(a) {
		return 0, false
	}
	return b.Sub(a), true
}
