package core

import (
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
	// Assessed: the tasks the agent rated; the rest are the average scores over them.
	Assessed     int     `json:"assessed"`
	Clarity      float64 `json:"clarity"`
	Complexity   float64 `json:"complexity"`
	Completeness float64 `json:"completeness"`
	Risk         float64 `json:"risk"`
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
}

type Reason struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// Summarize counts the records of the tasks that were taken; ones never handed off are not in it.
func Summarize(records []Record, now time.Time) Stats {
	st := Stats{Reasons: []Reason{}}
	reasons := map[string]int{}
	projects, executors := map[string]*Group{}, map[string]*Group{}
	var took []float64
	var review time.Duration
	reviewed := 0
	weekAgo := now.Add(-7 * 24 * time.Hour)
	for _, r := range records {
		if r.StartedAt == "" && r.Status != Failed {
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
		groups := []*Group{group(projects, project), group(executors, executor)}
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
			}
		}
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
	st.Projects, st.Executors = sorted(projects), sorted(executors)
	for text, n := range reasons {
		st.Reasons = append(st.Reasons, Reason{Text: text, Count: n})
	}
	sort.Slice(st.Reasons, func(i, j int) bool {
		a, b := st.Reasons[i], st.Reasons[j]
		return a.Count > b.Count || a.Count == b.Count && a.Text < b.Text
	})
	return st
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
