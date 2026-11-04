package core

import (
	"maps"
	"slices"
)

// Routes: the model and the effort each phase of a task runs with, by the size the estimate gave it. Mako
// picks them itself; a model picked for the task weighs more, a stuck task is escalated a level up.

// PhaseCIFix: the agent's run that fixes its MR's CI or its conflicts. Its spend goes to PhaseWork; only its route
// is kept on its own.
const PhaseCIFix = "cifix"

// RoutePhases: the phases with a route, in the order the settings show them.
var RoutePhases = []string{PhaseEstimate, PhasePlan, PhaseWork, PhaseReview, PhaseCIFix, PhaseAssess}

// Efforts: Claude Code's effort levels, the lowest first (claude --effort).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// modelLadder: the models, the cheapest first.
var modelLadder = []string{"haiku", "sonnet", "opus"}

// The route modes: the whole table a level down, as it is or a level up.
const (
	ModeCost     = "cost"
	ModeBalanced = "balanced"
	ModeQuality  = "quality"
)

// Modes: in the order the settings show them.
var Modes = []string{ModeCost, ModeBalanced, ModeQuality}

// ModeShift: how many levels mode moves the table.
func ModeShift(mode string) int {
	switch mode {
	case ModeCost:
		return -1
	case ModeQuality:
		return 1
	}
	return 0
}

// Route is a phase's model and effort; Why says how Mako came to it, for the card.
type Route struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Why    string `json:"why,omitempty"`
	// Size, Mode: the task's size and the routes' mode the route came from, for the stats; "" in the table.
	Size string `json:"size,omitempty"`
	Mode string `json:"mode,omitempty"`
	// BaseModel, BaseEffort: an escalated route's own, before it was lifted; "" for one not escalated.
	BaseModel  string `json:"baseModel,omitempty"`
	BaseEffort string `json:"baseEffort,omitempty"`
}

// Escalated: the route was lifted above its own (BaseModel, BaseEffort).
func (r Route) Escalated() bool { return r.BaseModel != "" || r.BaseEffort != "" }

// Label: "opus · high", "" for none.
func (r Route) Label() string {
	switch {
	case r.Model != "" && r.Effort != "":
		return r.Model + " · " + r.Effort
	case r.Model != "":
		return r.Model
	}
	return r.Effort
}

// LabelWithMode: Label, and "opus · high (quality)" when the mode moved the route; for the card, where the mode is
// not told otherwise. The logs keep Label: the route's Why there already names the mode.
func (r Route) LabelWithMode() string {
	label := r.Label()
	if label != "" && ModeShift(r.Mode) != 0 {
		label += " (" + r.Mode + ")"
	}
	return label
}

// RouteTable: the route of each phase by size.
type RouteTable map[string]map[string]Route

// Clone: a deep copy.
func (t RouteTable) Clone() RouteTable {
	c := RouteTable{}
	for p, bySize := range t {
		c[p] = maps.Clone(bySize)
	}
	return c
}

// DefaultRoutes: the table Mako starts with.
func DefaultRoutes() RouteTable {
	r := func(m, e string) Route { return Route{Model: m, Effort: e} }
	same := func(x Route) map[string]Route { return map[string]Route{"small": x, "medium": x, "big": x} }
	return RouteTable{
		PhaseEstimate: same(r("haiku", "medium")),
		PhaseAssess:   same(r("haiku", "medium")),
		PhasePlan:     {"small": r("sonnet", "medium"), "medium": r("sonnet", "high"), "big": r("opus", "high")},
		PhaseWork:     {"small": r("sonnet", "medium"), "medium": r("sonnet", "high"), "big": r("opus", "high")},
		PhaseReview:   {"small": r("sonnet", "medium"), "medium": r("sonnet", "high"), "big": r("opus", "medium")},
		PhaseCIFix:    {"small": r("sonnet", "medium"), "medium": r("sonnet", "medium"), "big": r("sonnet", "high")},
	}
}

// Step moves a route n levels: up, the effort rises to xhigh, then the model goes up; on the top model the
// effort goes on to max. Down, the effort falls to low, then the model goes down. A model or an effort Mako does
// not know is left as it is.
func Step(r Route, n int) Route {
	for ; n > 0; n-- {
		m, e := slices.Index(modelLadder, r.Model), slices.Index(Efforts, r.Effort)
		switch {
		case e >= 0 && e < slices.Index(Efforts, "xhigh"):
			r.Effort = Efforts[e+1]
		case m >= 0 && m < len(modelLadder)-1:
			r.Model = modelLadder[m+1]
		case e >= 0 && e < len(Efforts)-1:
			r.Effort = Efforts[e+1]
		}
	}
	for ; n < 0; n++ {
		m, e := slices.Index(modelLadder, r.Model), slices.Index(Efforts, r.Effort)
		switch {
		case e > 0:
			r.Effort = Efforts[e-1]
		case m > 0:
			r.Model = modelLadder[m-1]
		}
	}
	return r
}

// SetRoute keeps the route a phase of the task last ran with.
func (r *Record) SetRoute(phase string, route Route) {
	m := r.Metrics.clone()
	r.Metrics = m
	if m.Routes == nil {
		m.Routes = map[string]Route{}
	}
	m.Routes[phase] = route
}

// RouteOf: the route the phase last ran with (zero: none kept).
func (r *Record) RouteOf(phase string) Route {
	if r.Metrics == nil {
		return Route{}
	}
	return r.Metrics.Routes[phase]
}
