package core

import (
	"slices"
	"sort"
	"strings"
)

// RouteGroup is how one phase went for the tasks whose phase ran with one route: the phase, the task's size, the
// model and the effort. Every phase has results of its own (the groups of fields below); the ones of another
// phase stay zero. Taken and Cost are the phase's own for all of them.
type RouteGroup struct {
	Phase  string `json:"phase"`
	Size   string `json:"size"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Taken  int    `json:"taken"`
	// Older: of Taken, the tasks of older versions of Mako, which kept no effort: counted under the effort most
	// tasks of the same phase, size and model have; all of Taken when there is none.
	Older int `json:"older,omitempty"`
	// Cost: the phase's average USD over the Priced tasks (a CI fix's goes to the work: none of its own).
	Cost   float64 `json:"cost"`
	Priced int     `json:"priced"`

	// Work: Succeeded: an MR open or merged; Ended: merged or failed; Clean: merged with no rework (first pass).
	// MedianSeconds: the median from the hand-off to the MR, over the tasks that got one.
	Succeeded     int     `json:"succeeded,omitempty"`
	Ended         int     `json:"ended,omitempty"`
	Clean         int     `json:"clean,omitempty"`
	MedianSeconds float64 `json:"medianSeconds,omitempty"`

	// Estimate: against the assessment of the finished task. Compared: the tasks with both sizes, SizeHits: the
	// same size both times; RiskCompared, RiskHits: the same for the risk score.
	Compared     int `json:"compared,omitempty"`
	SizeHits     int `json:"sizeHits,omitempty"`
	RiskCompared int `json:"riskCompared,omitempty"`
	RiskHits     int `json:"riskHits,omitempty"`

	// Plan: PlanKnown: the plans whose approval was kept; PlanFirst: of them, approved with no «Поправити»;
	// PlanMedianSeconds: the median from the hand-off to the approval.
	PlanKnown         int     `json:"planKnown,omitempty"`
	PlanFirst         int     `json:"planFirst,omitempty"`
	PlanMedianSeconds float64 `json:"planMedianSeconds,omitempty"`

	// Agent review, over the tasks with a round (ReviewTasks): ReviewRounds, ReviewRemarks: the rounds and the
	// new remarks; ReviewFixed: remarks the agent answered "fixed"; ReviewQuick: tasks whose last round was clean
	// within ReviewRounds rounds.
	ReviewTasks   int `json:"reviewTasks,omitempty"`
	ReviewRounds  int `json:"reviewRounds,omitempty"`
	ReviewRemarks int `json:"reviewRemarks,omitempty"`
	ReviewFixed   int `json:"reviewFixed,omitempty"`
	ReviewQuick   int `json:"reviewQuick,omitempty"`

	// CI fix: CIEnded: the tasks merged or failed after a fix was asked; CIGreen: of them, merged after one
	// request; CIRequests: the requests over all Taken.
	CIEnded    int `json:"ciEnded,omitempty"`
	CIGreen    int `json:"ciGreen,omitempty"`
	CIRequests int `json:"ciRequests,omitempty"`
}

// routeKey: what tells one route from another.
type routeKey struct{ phase, size, model, effort string }

// routeAcc: a route's counts and the times its medians are of.
type routeAcc struct {
	RouteGroup
	seconds, planSeconds []float64
}

// Escalations: the tasks whose agent was lifted a level, and whether they were merged after it.
type Escalations struct {
	Tasks   int                `json:"tasks"`
	Ended   int                `json:"ended"`
	Merged  int                `json:"merged"`
	Reasons []EscalationReason `json:"reasons"`
}

// EscalationReason: the tasks lifted for one reason ("ci", "retry", "review"), a task once per reason.
type EscalationReason struct {
	Reason string `json:"reason"`
	Tasks  int    `json:"tasks"`
	Merged int    `json:"merged"`
}

// ModeGroup is how the tasks of one routes' mode went (by their work's route).
type ModeGroup struct {
	Mode      string  `json:"mode"`
	Taken     int     `json:"taken"`
	Succeeded int     `json:"succeeded"`
	Ended     int     `json:"ended"`
	Clean     int     `json:"clean"`
	Cost      float64 `json:"cost"`
}

// WhatIfModels: the models the same work is priced on, as if all of it ran on one.
var WhatIfModels = []string{"opus", "sonnet"}

// routeStats adds up the routes, the escalations and the what-if over the records Summarize counts.
type routeStats struct {
	prices  Prices
	groups  map[routeKey]*routeAcc
	esc     Escalations
	reasons map[string]*EscalationReason
	modes   map[string]*ModeGroup
	whatIf  map[string]float64
	actual  float64
	tasks   int
}

func newRouteStats(prices Prices) *routeStats {
	return &routeStats{prices: prices, groups: map[routeKey]*routeAcc{},
		reasons: map[string]*EscalationReason{}, modes: map[string]*ModeGroup{}, whatIf: map[string]float64{}}
}

// escalationReason: the kind of a reason in Record.EscalatedFor ("CI red ×2" → "ci", "review ×2" → "review").
func escalationReason(why string) string {
	w := strings.ToLower(strings.TrimSpace(why))
	for _, k := range []string{"ci", "retry", "review"} {
		if strings.HasPrefix(w, k) {
			return k
		}
	}
	if w == "" {
		return "—"
	}
	return w
}

// routeKind: the route's size and mode; a route kept before they were (only Why says them: "big, cost, …",
// "medium (no estimate)") is read from Why.
func routeKind(route Route) (size, mode string) {
	size, mode = route.Size, route.Mode
	if size != "" || route.Why == "" {
		return size, mode
	}
	parts := strings.Split(route.Why, ", ")
	if s := strings.TrimSuffix(parts[0], " (no estimate)"); slices.Contains(Sizes, s) {
		size, mode = s, ModeBalanced
	}
	for _, p := range parts[1:] {
		if p == ModeCost || p == ModeQuality {
			mode = p
		}
	}
	return size, mode
}

// ownRoute: the model and the effort the task's phase is counted under: an escalated one's own, from before it
// was lifted, so a task that failed on a route counts against that route and not the one it was lifted to. A
// route escalated before its own was kept (only Why says so) cannot be told: not counted (ok false).
func ownRoute(route Route) (model, effort string, ok bool) {
	switch {
	case route.Escalated():
		return route.BaseModel, route.BaseEffort, true
	case strings.Contains(route.Why, "escalated:"):
		return "", "", false
	}
	return route.Model, route.Effort, true
}

// estimate counts the estimate of r against its assessment: the size and the risk it expected and the ones the
// finished task came to.
func (g *routeAcc) estimate(r Record) {
	e, a := r.Estimate, r.Assessment
	if e == nil || e.State != "done" || a == nil || a.State != "done" {
		return
	}
	if want, got := slices.Index(Sizes, e.Size), slices.Index(Sizes, a.Size); want >= 0 && got >= 0 {
		g.Compared++
		if want == got {
			g.SizeHits++
		}
	}
	if e.Risk.Value > 0 && a.Risk.Value > 0 {
		g.RiskCompared++
		if e.Risk.Value == a.Risk.Value {
			g.RiskHits++
		}
	}
}

// review counts the reviewer's rounds over r: the ones that read the branch to the end, as reviewStats does.
func (g *routeAcc) review(r Record) {
	rounds := 0
	for _, a := range r.AgentReviews {
		if a.State != "notes" && a.State != "clean" {
			continue
		}
		rounds++
		for _, m := range a.Remarks {
			if m.Again {
				continue
			}
			g.ReviewRemarks++
			if m.Reply == Fixed {
				g.ReviewFixed++
			}
		}
	}
	if rounds == 0 {
		return
	}
	g.ReviewTasks++
	g.ReviewRounds += rounds
	if r.AgentReviews[len(r.AgentReviews)-1].State == "clean" && rounds <= ReviewRounds {
		g.ReviewQuick++
	}
}

// add: the counts of o on top of g's (o's key fields stay out of it).
func (g *routeAcc) add(o *routeAcc) {
	g.Taken += o.Taken
	g.Cost += o.Cost
	g.Priced += o.Priced
	g.Succeeded += o.Succeeded
	g.Ended += o.Ended
	g.Clean += o.Clean
	g.Compared += o.Compared
	g.SizeHits += o.SizeHits
	g.RiskCompared += o.RiskCompared
	g.RiskHits += o.RiskHits
	g.PlanKnown += o.PlanKnown
	g.PlanFirst += o.PlanFirst
	g.ReviewTasks += o.ReviewTasks
	g.ReviewRounds += o.ReviewRounds
	g.ReviewRemarks += o.ReviewRemarks
	g.ReviewFixed += o.ReviewFixed
	g.ReviewQuick += o.ReviewQuick
	g.CIEnded += o.CIEnded
	g.CIGreen += o.CIGreen
	g.CIRequests += o.CIRequests
	g.seconds = append(g.seconds, o.seconds...)
	g.planSeconds = append(g.planSeconds, o.planSeconds...)
}

// foldOlder: the routes of older versions of Mako, which kept no effort, counted under the route of the same phase,
// size and model that most tasks have an effort for, so they do not look like a route of their own; with none
// they stay as they are. Older says how many tasks of a route are such.
func (s *routeStats) foldOlder() {
	var keys []routeKey
	for key := range s.groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { // stable: the same input, the same fold
		a, b := keys[i], keys[j]
		return a.phase+a.size+a.model+a.effort < b.phase+b.size+b.model+b.effort
	})
	for _, key := range keys {
		if key.effort != "" {
			continue
		}
		old := s.groups[key]
		var into *routeAcc
		for _, k := range keys {
			if g := s.groups[k]; k.effort != "" && k.phase == key.phase && k.size == key.size && k.model == key.model &&
				(into == nil || g.Taken > into.Taken) {
				into = g
			}
		}
		if into == nil {
			old.Older = old.Taken
			continue
		}
		into.add(old)
		into.Older += old.Taken
		delete(s.groups, key)
	}
}

func (s *routeStats) add(r Record) {
	failed := r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed"
	merged := r.Status == Done && r.ReviewState == "merged"
	succeeded := r.Status == Done
	firstPass := merged && clean(r)
	took, timed := between(r.StartedAt, r.FinishedAt)
	timed = timed && succeeded

	if r.Escalation > 0 {
		s.esc.Tasks++
		if failed || merged {
			s.esc.Ended++
		}
		if merged {
			s.esc.Merged++
		}
		seen := map[string]bool{}
		for _, why := range r.EscalatedFor {
			k := escalationReason(why)
			if seen[k] {
				continue
			}
			seen[k] = true
			if s.reasons[k] == nil {
				s.reasons[k] = &EscalationReason{Reason: k}
			}
			s.reasons[k].Tasks++
			if merged {
				s.reasons[k].Merged++
			}
		}
	}

	m := r.Metrics
	if m == nil {
		return
	}
	for phase, route := range m.Routes {
		model, effort, ok := ownRoute(route)
		if !ok {
			continue
		}
		size, _ := routeKind(route)
		key := routeKey{phase, size, ModelFamily(model), effort}
		if key.size == "" {
			key.size = "—"
		}
		g := s.groups[key]
		if g == nil {
			g = &routeAcc{RouteGroup: RouteGroup{Phase: phase, Size: key.size, Model: key.model, Effort: effort}}
			s.groups[key] = g
		}
		g.Taken++
		if usd := m.Cost[phase]; usd > 0 && phase != PhaseCIFix {
			g.Cost += usd
			g.Priced++
		}
		switch phase {
		case PhaseWork:
			if succeeded {
				g.Succeeded++
			}
			if failed || merged {
				g.Ended++
			}
			if firstPass {
				g.Clean++
			}
			if timed {
				g.seconds = append(g.seconds, took.Seconds())
			}
		case PhaseEstimate:
			g.estimate(r)
		case PhasePlan:
			if r.PlanApprovedAt != "" {
				g.PlanKnown++
				if r.PlanRevisions == 0 {
					g.PlanFirst++
				}
				if d, ok := between(r.StartedAt, r.PlanApprovedAt); ok {
					g.planSeconds = append(g.planSeconds, d.Seconds())
				}
			}
		case PhaseReview:
			g.review(r)
		case PhaseCIFix:
			g.CIRequests += r.CIFixes
			if r.CIFixes > 0 && (failed || merged) {
				g.CIEnded++
				if merged && r.CIFixes == 1 {
					g.CIGreen++
				}
			}
		}
	}
	route, ok := m.Routes[PhaseWork]
	if !ok {
		route, ok = m.Routes[PhasePlan]
	}
	if _, mode := routeKind(route); ok && mode != "" {
		g := s.modes[mode]
		if g == nil {
			g = &ModeGroup{Mode: mode}
			s.modes[mode] = g
		}
		g.Taken++
		g.Cost += r.Spent()
		if succeeded {
			g.Succeeded++
		}
		if failed || merged {
			g.Ended++
		}
		if firstPass {
			g.Clean++
		}
	}
	// What-if: only the calls whose tokens are known by kind, against what the same calls cost (UsageCost: a
	// phase's Cost also has the calls from before the tokens were kept by kind).
	counted := false
	for phase, u := range m.Usage {
		if u.Total() == 0 {
			continue
		}
		counted = true
		s.actual += m.UsageCost[phase]
		for _, model := range WhatIfModels {
			s.whatIf[model] += s.prices.Cost(model, u)
		}
	}
	if counted {
		s.tasks++
	}
}

func (s *routeStats) done(st *Stats) {
	s.foldOlder()
	st.Routes = make([]RouteGroup, 0, len(s.groups))
	for _, g := range s.groups {
		if g.Priced > 0 {
			g.Cost /= float64(g.Priced)
		}
		g.MedianSeconds = median(g.seconds)
		g.PlanMedianSeconds = median(g.planSeconds)
		st.Routes = append(st.Routes, g.RouteGroup)
	}
	order := func(xs []string, x string) int {
		if i := slices.Index(xs, x); i >= 0 {
			return i
		}
		return len(xs)
	}
	sort.Slice(st.Routes, func(i, j int) bool {
		a, b := st.Routes[i], st.Routes[j]
		if pa, pb := order(RoutePhases, a.Phase), order(RoutePhases, b.Phase); pa != pb {
			return pa < pb
		}
		if sa, sb := order(Sizes, a.Size), order(Sizes, b.Size); sa != sb {
			return sa < sb
		}
		if a.Model != b.Model {
			return order(modelLadder, a.Model) < order(modelLadder, b.Model)
		}
		return order(Efforts, a.Effort) < order(Efforts, b.Effort)
	})
	st.Escalations = s.esc
	st.Escalations.Reasons = make([]EscalationReason, 0, len(s.reasons))
	for _, r := range s.reasons {
		st.Escalations.Reasons = append(st.Escalations.Reasons, *r)
	}
	sort.Slice(st.Escalations.Reasons, func(i, j int) bool {
		a, b := st.Escalations.Reasons[i], st.Escalations.Reasons[j]
		return a.Tasks > b.Tasks || a.Tasks == b.Tasks && a.Reason < b.Reason
	})
	st.Modes = make([]ModeGroup, 0, len(s.modes))
	for _, g := range s.modes {
		st.Modes = append(st.Modes, *g)
	}
	sort.Slice(st.Modes, func(i, j int) bool { return order(Modes, st.Modes[i].Mode) < order(Modes, st.Modes[j].Mode) })
	st.WhatIf, st.WhatIfActual, st.WhatIfTasks = s.whatIf, s.actual, s.tasks
}
