package core

import (
	"slices"
	"sort"
	"strings"
)

// RouteGroup is how the tasks went whose phase ran with one route: the phase, the task's size, the model and
// the effort.
type RouteGroup struct {
	Phase  string `json:"phase"`
	Size   string `json:"size"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Taken  int    `json:"taken"`
	// Succeeded: an MR open or merged; Ended: merged or failed; Clean: merged with no rework (first pass).
	Succeeded int `json:"succeeded"`
	Ended     int `json:"ended"`
	Clean     int `json:"clean"`
	// Cost: the phase's average USD over the Priced tasks (a CI fix's goes to the work: none of its own).
	Cost   float64 `json:"cost"`
	Priced int     `json:"priced"`
	// MedianSeconds: the median from the hand-off to the MR, over the tasks that got one.
	MedianSeconds float64 `json:"medianSeconds"`
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
	groups  map[RouteGroup]*RouteGroup // by the key fields only
	seconds map[RouteGroup][]float64
	esc     Escalations
	reasons map[string]*EscalationReason
	modes   map[string]*ModeGroup
	whatIf  map[string]float64
	actual  float64
	tasks   int
}

func newRouteStats(prices Prices) *routeStats {
	return &routeStats{prices: prices, groups: map[RouteGroup]*RouteGroup{}, seconds: map[RouteGroup][]float64{},
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
		size, _ := routeKind(route)
		key := RouteGroup{Phase: phase, Size: size, Model: ModelFamily(route.Model), Effort: route.Effort}
		if key.Size == "" {
			key.Size = "—"
		}
		g := s.groups[key]
		if g == nil {
			g = &RouteGroup{Phase: key.Phase, Size: key.Size, Model: key.Model, Effort: key.Effort}
			s.groups[key] = g
		}
		g.Taken++
		if succeeded {
			g.Succeeded++
		}
		if failed || merged {
			g.Ended++
		}
		if firstPass {
			g.Clean++
		}
		if usd := m.Cost[phase]; usd > 0 && phase != PhaseCIFix {
			g.Cost += usd
			g.Priced++
		}
		if timed {
			s.seconds[key] = append(s.seconds[key], took.Seconds())
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
	// What-if: only the phases whose tokens are known by kind, against what the same phases cost.
	counted := false
	for phase, u := range m.Usage {
		if u.Total() == 0 {
			continue
		}
		counted = true
		s.actual += m.Cost[phase]
		for _, model := range WhatIfModels {
			s.whatIf[model] += s.prices.Cost(model, u)
		}
	}
	if counted {
		s.tasks++
	}
}

func (s *routeStats) done(st *Stats) {
	st.Routes = make([]RouteGroup, 0, len(s.groups))
	for key, g := range s.groups {
		if g.Priced > 0 {
			g.Cost /= float64(g.Priced)
		}
		g.MedianSeconds = median(s.seconds[key])
		st.Routes = append(st.Routes, *g)
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
