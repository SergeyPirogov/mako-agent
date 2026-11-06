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
	// Cancelled: of the Failed, the ones stopped on purpose rather than gone wrong.
	Cancelled int `json:"cancelled"`
	// Ended: merged or failed, whose outcome is known; Clean: merged with no rework, as in SizeGroup.
	Ended int `json:"ended"`
	Clean int `json:"clean"`
	// LeadSeconds: the median from the hand-off to the merge, over the merged tasks.
	LeadSeconds float64 `json:"leadSeconds"`
	// Days: the tasks taken on each day of the period (the last 14 days for all time), the oldest first.
	Days []Day `json:"days"`
	// Stages: where the time of a merged task went, on average by stage (Metrics), over the Staged ones.
	Stages map[string]float64 `json:"stages"`
	Staged int                `json:"staged"`
	// Phases: USD by phase (estimate, plan, work, review, assess); PhaseTokens and Tokens: the tokens so.
	Phases      map[string]float64 `json:"phases"`
	PhaseTokens map[string]int     `json:"phaseTokens"`
	Tokens      int                `json:"tokens"`
	// CIFixes, ReviewFixes: the rounds the agent was asked to fix CI and the review's comments, on average
	// per merged task.
	CIFixes     float64 `json:"ciFixes"`
	ReviewFixes float64 `json:"reviewFixes"`
	// Context: how the agents' context filled, by phase.
	Context ContextStats `json:"context"`
	// Review: how the reviewer agent's rounds went: remarks, the agent's answers, the rounds to a clean one.
	Review ReviewStats `json:"review"`
	// Routes: how the tasks went by the route of each phase (phase · size → model · effort).
	Routes []RouteGroup `json:"routes"`
	// Escalations: the tasks whose agent was lifted a level, and why.
	Escalations Escalations `json:"escalations"`
	// WhatIf: what the phases with tokens known by kind would cost all on one model (WhatIfModels), at list
	// prices; WhatIfActual: what they did cost; WhatIfTasks: the tasks they are of.
	WhatIf       map[string]float64 `json:"whatIf"`
	WhatIfActual float64            `json:"whatIfActual"`
	WhatIfTasks  int                `json:"whatIfTasks"`
	// Modes: by the routes' mode of the task's work.
	Modes []ModeGroup `json:"modes"`
	// SelfCheck: the merged tasks with «Check it yourself», checked or not, and their fixes after the merge.
	SelfCheck SelfCheckStats `json:"selfCheck"`
	// RiskLevels: the merged tasks by the change's risk, the auto-merges and their fixes after.
	RiskLevels RiskStats `json:"riskLevels"`
}

// Day is how the tasks taken on one day went.
type Day struct {
	Date   string  `json:"date"` // 2006-01-02, local
	Taken  int     `json:"taken"`
	Merged int     `json:"merged"`
	Failed int     `json:"failed"`
	Cost   float64 `json:"cost"`
}

// trendDays: the days the trend shows for all time, and the most it shows for a period.
const trendDays, maxTrendDays = 14, 90

// Forecast compares the estimate before the start with the assessment after the MR, over the tasks that have
// both sizes.
type Forecast struct {
	Compared int `json:"compared"`
	Hits     int `json:"hits"` // the same size both times
	// Matrix[expected][came to]: how many tasks, both in the order of Sizes.
	Matrix [][]int `json:"matrix"`
	// Under: the estimate expected a smaller size than the task came to; Over: a bigger one.
	Under int `json:"under"`
	Over  int `json:"over"`
	// ByClarity: how the estimated tasks went by the clarity the estimate gave their description, 1 to 5.
	ByClarity []ClarityGroup `json:"byClarity"`
}

// ClarityGroup is how the tasks whose estimate found their description this clear went.
type ClarityGroup struct {
	Clarity int `json:"clarity"`
	// Ended: merged or failed; Merged; Clean: merged with no rework, as in SizeGroup.
	Ended  int `json:"ended"`
	Merged int `json:"merged"`
	Clean  int `json:"clean"`
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
	// Tokens: the average over the Tokened tasks.
	Tokens  float64 `json:"tokens"`
	Tokened int     `json:"tokened"`
	// Per100Lines: USD per 100 changed lines (added and removed) over the Lined merged tasks with a price and a
	// change kept: what a unit of work costs, so sizes compare by the work, not by the task.
	Per100Lines float64 `json:"per100Lines,omitempty"`
	Lined       int     `json:"lined,omitempty"`
	lineCost    float64
	lines       int
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
	Tokens  int `json:"tokens"`
}

// ModelFamily: the model by its family, so Claude Code's own name of it and the alias Mako asked for count as
// one ("claude-opus-4-7[1m]" and "opus" are "opus"); another name without "claude-" and "[…]"; "—" unknown.
func ModelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(m, "["); i >= 0 {
		m = m[:i]
	}
	for _, f := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(m, f) {
			return f
		}
	}
	if m = strings.TrimPrefix(m, "claude-"); m == "" {
		return "—"
	}
	return m
}

// unclear: the highest clarity of a task the agent hardly understood.
const unclear = 2

type Reason struct {
	// Key: what the failures are counted by: their ErrorKind, else the first line of their error.
	Key   string `json:"key"`
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// Summarize counts the records of the tasks that were taken since (zero: all of them); ones never handed off
// are not in it. The what-if is at the default prices.
func Summarize(records []Record, now, since time.Time) Stats {
	return SummarizePriced(records, now, since, DefaultPrices())
}

// SummarizePriced is Summarize with the what-if at prices.
func SummarizePriced(records []Record, now, since time.Time, prices Prices) Stats {
	st := Stats{Reasons: []Reason{}, Stages: map[string]float64{}, Phases: map[string]float64{}, PhaseTokens: map[string]int{}, Forecast: Forecast{Matrix: make([][]int, len(Sizes)), ByClarity: make([]ClarityGroup, 5)}, BySize: make([]SizeGroup, len(Sizes))}
	for i := range st.Forecast.ByClarity {
		st.Forecast.ByClarity[i].Clarity = i + 1
	}
	for i, size := range Sizes {
		st.Forecast.Matrix[i] = make([]int, len(Sizes))
		st.BySize[i].Size = size
	}
	reasons := map[string]*Reason{}
	projects, executors, authors, models := map[string]*Group{}, map[string]*Group{}, map[string]*Group{}, map[string]*Group{}
	var took, lead []float64
	var review time.Duration
	reviewed := 0
	weekAgo := now.Add(-7 * 24 * time.Hour)
	days := trend(now, since)
	ctxs := newContextStats()
	revs := &reviewStats{}
	if prices == nil {
		prices = DefaultPrices()
	}
	routes := newRouteStats(prices)
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
		project := projectOf(r)
		executor := r.Executor
		if executor == "" {
			executor = "—"
		}
		model := ModelFamily(r.AgentModel)
		groups := []*Group{group(projects, project), group(executors, executor), group(models, model)}
		if r.Task.Author != "" {
			groups = append(groups, group(authors, r.Task.Author))
		}
		if r.Cost > 0 {
			st.Cost += r.Cost
			st.Priced++
		}
		tokens := r.TokensUsed()
		st.Tokens += tokens
		for _, g := range groups {
			g.Taken++
			g.Cost += r.Cost
			g.Tokens += tokens
		}
		if m := r.Metrics; m != nil {
			for phase, usd := range m.Cost {
				st.Phases[phase] += usd
			}
			for phase, n := range m.Tokens {
				st.PhaseTokens[phase] += n
			}
		}
		ctxs.add(r)
		revs.add(r)
		routes.add(r)
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
		day := days[localDay(r.StartedAt, now.Location())]
		if day != nil {
			day.Taken++
			day.Cost += r.Cost
		}
		switch {
		case r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed":
			st.Failed++
			st.Ended++
			if day != nil {
				day.Failed++
			}
			if cancelled(r) {
				st.Cancelled++
			}
			key, text := reasonOf(r)
			if reasons[key] == nil {
				reasons[key] = &Reason{Key: key, Text: text}
			}
			reasons[key].Count++
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
				st.Ended++
				if day != nil {
					day.Merged++
				}
				if clean(r) {
					st.Clean++
				}
				st.CIFixes += float64(r.CIFixes)
				st.ReviewFixes += float64(r.ReviewFixes)
				if r.Metrics != nil {
					st.Staged++
					for stage, sec := range r.StageSeconds(now) {
						st.Stages[stage] += sec
					}
				}
				if d, ok := between(r.FinishedAt, r.MergedAt); ok {
					review += d
					reviewed++
				}
				if d, ok := between(r.StartedAt, r.MergedAt); ok {
					lead = append(lead, d.Seconds())
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
		st.MedianSeconds = median(took)
	}
	st.LeadSeconds = median(lead)
	st.Context = ctxs.done()
	st.Review = revs.done()
	st.SelfCheck = selfCheckStats(records, since)
	st.RiskLevels = riskStats(records, since)
	routes.done(&st)
	if st.Merged > 0 {
		st.CIFixes /= float64(st.Merged)
		st.ReviewFixes /= float64(st.Merged)
	}
	if st.Staged > 0 {
		for stage := range st.Stages {
			st.Stages[stage] /= float64(st.Staged)
		}
	}
	st.Days = make([]Day, 0, len(days))
	for _, d := range days {
		st.Days = append(st.Days, *d)
	}
	sort.Slice(st.Days, func(i, j int) bool { return st.Days[i].Date < st.Days[j].Date })
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
		if g.Tokened > 0 {
			g.Tokens /= float64(g.Tokened)
		}
		if g.lines > 0 {
			g.Per100Lines = g.lineCost / float64(g.lines) * 100
		}
	}
	st.Projects, st.Executors, st.Authors, st.Models = sorted(projects), sorted(executors), sorted(authors), sorted(models)
	for _, x := range reasons {
		st.Reasons = append(st.Reasons, *x)
	}
	sort.Slice(st.Reasons, func(i, j int) bool {
		a, b := st.Reasons[i], st.Reasons[j]
		return a.Count > b.Count || a.Count == b.Count && a.Text < b.Text
	})
	return st
}

// projectOf: the project a record is counted under: its task's, else its repository.
func projectOf(r Record) string {
	if r.Task.Project != "" {
		return r.Task.Project
	}
	return r.Repo
}

// OfProject: the records of one project, as Summarize names it; all of them for "".
func OfProject(records []Record, project string) []Record {
	if project == "" {
		return records
	}
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if projectOf(r) == project {
			out = append(out, r)
		}
	}
	return out
}

// bySize counts r by the size its estimate expected, and against the size it came to when it has both.
func bySize(st *Stats, r Record) {
	e := r.Estimate
	if e == nil || e.State != "done" {
		return
	}
	failed := r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed"
	merged := r.Status == Done && r.ReviewState == "merged"
	if v := e.Clarity.Value; v >= 1 && v <= len(st.Forecast.ByClarity) && (failed || merged) {
		c := &st.Forecast.ByClarity[v-1]
		c.Ended++
		if merged {
			c.Merged++
			if clean(r) {
				c.Clean++
			}
		}
	}
	want := slices.Index(Sizes, e.Size)
	if want < 0 {
		return
	}
	if a := r.Assessment; a != nil && a.State == "done" {
		if got := slices.Index(Sizes, a.Size); got >= 0 {
			st.Forecast.Compared++
			st.Forecast.Matrix[want][got]++
			switch {
			case got == want:
				st.Forecast.Hits++
			case got > want:
				st.Forecast.Under++
			default:
				st.Forecast.Over++
			}
		}
	}
	g := &st.BySize[want]
	g.Taken++
	if failed || merged {
		g.Ended++
	}
	if merged && clean(r) {
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
	if n := r.TokensUsed(); n > 0 {
		g.Tokens += float64(n)
		g.Tokened++
	}
	if ch := r.Changed; merged && r.Cost > 0 && ch != nil && ch.Added+ch.Removed > 0 {
		g.lineCost += r.Cost
		g.lines += ch.Added + ch.Removed
		g.Lined++
	}
}

// clean: merged with no rework: no «Повторити інакше» or another attempt, no fixes after a review.
func clean(r Record) bool {
	return r.Attempt <= 1 && r.ReviewFixes == 0 && !reviewed(r)
}

// cancelled: stopped on purpose, from the board or in Orca, rather than gone wrong. A failed task whose error only
// reads "Cancelled" is not: the board's cancel always ends in the cancelled status.
func cancelled(r Record) bool {
	return ErrorKindOf(r) == KindCancelled && (r.Status == Cancelled || strings.Contains(strings.ToLower(r.Error), "cancelled by the developer"))
}

// trend: an empty day for each day the trend shows, by its local date: the period's (at least one, so "today"
// shows today), the last 14 for all time.
func trend(now, since time.Time) map[string]*Day {
	n := trendDays
	if !since.IsZero() {
		n = max(1, min(int(now.Sub(since).Hours()/24+0.5), maxTrendDays))
	}
	days := map[string]*Day{}
	for i := 0; i < n; i++ {
		date := now.AddDate(0, 0, -i).Format(time.DateOnly)
		days[date] = &Day{Date: date}
	}
	return days
}

// localDay: the local date of a RFC 3339 time, "" when there is none.
func localDay(at string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return ""
	}
	return t.In(loc).Format(time.DateOnly)
}

func median(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	sort.Float64s(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
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

// reasonOf: what a failed record is counted by, and the words for it: a known cause by its kind in the interface's
// language, however its error was worded; any other by the first line of its error.
func reasonOf(r Record) (key, text string) {
	if label := KindLabel(ErrorKindOf(r)); label != "" {
		return ErrorKindOf(r), label
	}
	text = reason(r.Error)
	return text, text
}

// reason: the first line of the error, short, so the same failure counts once.
func reason(err string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(err), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "No reason"
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
