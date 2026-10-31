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
}

type Reason struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// Summarize counts the records of the tasks that were taken; ones never handed off are not in it.
func Summarize(records []Record) Stats {
	st := Stats{Reasons: []Reason{}}
	reasons := map[string]int{}
	var took time.Duration
	timed := 0
	for _, r := range records {
		if r.StartedAt == "" && r.Status != Failed {
			continue
		}
		st.Taken++
		if r.Cost > 0 {
			st.Cost += r.Cost
			st.Priced++
		}
		switch {
		case r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed":
			st.Failed++
			reasons[reason(r.Error)]++
		case r.Status == Done:
			st.Succeeded++
			if r.ReviewState == "merged" {
				st.Merged++
			}
			if d, ok := between(r.StartedAt, r.FinishedAt); ok {
				took += d
				timed++
			}
		default:
			st.Running++
		}
	}
	if timed > 0 {
		st.AvgSeconds = took.Seconds() / float64(timed)
	}
	for text, n := range reasons {
		st.Reasons = append(st.Reasons, Reason{Text: text, Count: n})
	}
	sort.Slice(st.Reasons, func(i, j int) bool {
		a, b := st.Reasons[i], st.Reasons[j]
		return a.Count > b.Count || a.Count == b.Count && a.Text < b.Text
	})
	return st
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
