package core

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// How to spend less: what the stats tell to change, for the Cost tab and the routes' table in the settings, which
// marks the cell to change and offers its new value.

// AdviceMin: the finished tasks a route needs before it is compared or judged.
const AdviceMin = 3

// Advice is one thing to change.
type Advice struct {
	// Kind: "warn" (a route to change), "tip" (how the tasks are given), "wait" (a route changed since, with no
	// finished task on it yet).
	Kind  string      `json:"kind"`
	Title string      `json:"title"`
	Text  string      `json:"text"`
	Rows  []AdviceRow `json:"rows,omitempty"`
	// Save: what the period would have cost less, USD; 0 when it cannot be told.
	Save float64 `json:"save,omitempty"`
	// Phase, Size: the cell of the routes' table it is about; Set: its value to set, as the table keeps it (before
	// the mode moves it), when the advice is to change it.
	Phase string `json:"phase,omitempty"`
	Size  string `json:"size,omitempty"`
	Set   *Route `json:"set,omitempty"`
}

// AdviceRow: a route or a size compared, its figures, and whether it is the one in use.
type AdviceRow struct {
	Label   string `json:"label"`
	Figures string `json:"figures"`
	Now     bool   `json:"now,omitempty"`
}

// CostAdvice: what to change, the biggest saving first. now: the route each phase and size gets (the table with
// mode applied), mode: the routes' mode.
func CostAdvice(st Stats, now RouteTable, mode string) []Advice {
	var out []Advice
	for _, size := range Sizes {
		if a, ok := workAdvice(st.Routes, size, now[PhaseWork][size], mode); ok {
			out = append(out, a)
		}
	}
	if a, ok := bigTasksAdvice(st.BySize); ok {
		out = append(out, a)
	}
	rank := func(a Advice) int { return map[string]int{"warn": 0, "tip": 1, "wait": 2}[a.Kind] }
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i].Save > out[j].Save
	})
	return out
}

func routeName(model, effort string) string {
	if model == "" || model == "—" {
		model = "default"
	}
	name := strings.ToUpper(model[:1]) + model[1:]
	if effort != "" {
		name += " · " + effort
	}
	return name
}

func firstPassOf(g RouteGroup) float64 { return float64(g.Clean) / float64(g.Ended) }

func passText(clean, ended int) string {
	return fmt.Sprintf("first pass %d of %d (%d%%)", clean, ended, int(math.Round(float64(clean)/float64(ended)*100)))
}

func routeRow(g RouteGroup, now bool) AdviceRow {
	return AdviceRow{Label: routeName(g.Model, g.Effort), Figures: fmt.Sprintf("$%.2f a task · %s", g.Cost, passText(g.Clean, g.Ended)), Now: now}
}

// workAdvice: the Work route in use for the size against the others tried for it: one that does better, or as
// well for less, is advised; a weak one in use with nothing better tried is pointed out; a route changed since,
// with no finished task yet, is said to be watched.
func workAdvice(routes []RouteGroup, size string, want Route, mode string) (Advice, bool) {
	var gs []RouteGroup
	for _, g := range routes {
		if g.Phase == PhaseWork && g.Size == size && g.Ended >= AdviceMin && g.Priced > 0 {
			gs = append(gs, g)
		}
	}
	where := strings.ToUpper(size[:1]) + size[1:] + " tasks · Work"
	var cur *RouteGroup
	for i := range gs {
		if gs[i].Model == want.Model && gs[i].Effort == want.Effort {
			cur = &gs[i]
		}
	}
	if cur == nil {
		if len(gs) == 0 || want.Model == "" {
			return Advice{}, false
		}
		// The route in use may have a few tasks ended already, too few (or with no price) to be compared.
		ended := 0
		for _, g := range routes {
			if g.Phase == PhaseWork && g.Size == size && g.Model == want.Model && g.Effort == want.Effort {
				ended += g.Ended
			}
		}
		so := "no finished task on it yet"
		if ended > 0 {
			so = fmt.Sprintf("%d of %d finished tasks on it", ended, AdviceMin)
		}
		return Advice{Kind: "wait", Phase: PhaseWork, Size: size,
			Title: fmt.Sprintf("%s: now %s, %s", where, routeName(want.Model, want.Effort), so),
			Text:  fmt.Sprintf("Mako compares it with the routes tried before once %d tasks have ended on it.", AdviceMin),
		}, true
	}
	var alt *RouteGroup
	var better, cheaper bool
	for i := range gs {
		g := &gs[i]
		if g == cur {
			continue
		}
		b, w, c := firstPassOf(*g)-firstPassOf(*cur) >= 0.1, firstPassOf(*cur)-firstPassOf(*g) >= 0.1, g.Cost <= cur.Cost*0.8
		if w || !(b || c) {
			continue
		}
		if alt == nil || g.Cost < alt.Cost {
			alt, better, cheaper = g, b, c
		}
	}
	if alt != nil {
		how := "is cheaper, as good"
		switch {
		case better && cheaper:
			how = "is cheaper and does better"
		case better:
			how = "does better"
		}
		save := math.Max(0, (cur.Cost-alt.Cost)*float64(cur.Taken))
		text := "Rework costs more than the dearer route: fewer rounds, fewer runs."
		if save > 0 {
			text = fmt.Sprintf("On %s the %d tasks of this period would have cost about $%.2f less.", routeName(alt.Model, alt.Effort), cur.Taken, save)
		}
		return Advice{Kind: "warn", Phase: PhaseWork, Size: size, Save: save, Text: text,
			Title: fmt.Sprintf("%s: %s %s", where, routeName(alt.Model, alt.Effort), how),
			Rows:  []AdviceRow{routeRow(*cur, true), routeRow(*alt, false)},
			Set:   tableRoute(Route{Model: alt.Model, Effort: alt.Effort}, mode),
		}, true
	}
	if firstPassOf(*cur) < 0.5 {
		return Advice{Kind: "warn", Phase: PhaseWork, Size: size,
			Title: fmt.Sprintf("%s: %s gets %d%% through first time", where, routeName(cur.Model, cur.Effort), int(math.Round(firstPassOf(*cur)*100))),
			Text:  "Each rework is another run: a level up, or smaller tasks, usually costs less than the rounds.",
			Rows:  []AdviceRow{routeRow(*cur, true)},
			Set:   tableRoute(Step(Route{Model: cur.Model, Effort: cur.Effort}, 1), mode),
		}, true
	}
	return Advice{}, false
}

// tableRoute: the value of the routes' table that mode moves to the route a task should get; nil when Mako does not
// know its model or effort, or no value of the table leads to it in this mode. Step does not undo itself at the
// ends of the ladder (cost: sonnet · max, not opus · xhigh, gives sonnet · xhigh), so the value is checked, and
// looked for when the step back misses.
func tableRoute(effective Route, mode string) *Route {
	if !slices.Contains(modelLadder, effective.Model) || !slices.Contains(Efforts, effective.Effort) {
		return nil
	}
	shift := ModeShift(mode)
	gives := func(r Route) bool { return sameRoute(Step(r, shift), effective) }
	if r := Step(effective, -shift); gives(r) {
		return &Route{Model: r.Model, Effort: r.Effort}
	}
	for _, m := range modelLadder {
		for _, e := range Efforts {
			if r := (Route{Model: m, Effort: e}); gives(r) {
				return &r
			}
		}
	}
	return nil
}

func sameRoute(a, b Route) bool { return a.Model == b.Model && a.Effort == b.Effort }

// AdviceLinedMin: the merged tasks with their changed lines each size needs before sizes are compared by the work.
const AdviceLinedMin = 5

// bigTasksAdvice: big tasks are worth splitting when a unit of their work (100 changed lines) costs 1.25× a medium
// task's or more: comparing by the task would weigh the bigger work, not the waste. The saving is what the
// period's big tasks would have cost at the medium price of the work.
func bigTasksAdvice(sizes []SizeGroup) (Advice, bool) {
	by := map[string]SizeGroup{}
	for _, g := range sizes {
		by[g.Size] = g
	}
	big, med := by["big"], by["medium"]
	if big.Lined < AdviceLinedMin || med.Lined < AdviceLinedMin || med.Per100Lines == 0 {
		return Advice{}, false
	}
	ratio := big.Per100Lines / med.Per100Lines
	if ratio < 1.25 {
		return Advice{}, false
	}
	row := func(label string, g SizeGroup) AdviceRow {
		f := fmt.Sprintf("$%.2f per 100 changed lines · $%.2f a task", g.Per100Lines, g.Cost)
		if g.Ended > 0 {
			f += " · " + passText(g.Clean, g.Ended)
		}
		return AdviceRow{Label: label, Figures: f}
	}
	save := big.Cost * float64(big.Priced) * (1 - 1/ratio)
	return Advice{Kind: "tip", Save: save,
		Title: fmt.Sprintf("Big tasks cost %.1f× as much per line of work", ratio),
		Rows:  []AdviceRow{row("Big", big), row("Medium", med)},
		Text: fmt.Sprintf("The same work in medium tasks would have cost about $%.2f less in this period (%d%%): fewer reworks, shorter "+
			"sessions. Split a big task before the hand-off; the estimate marks the ones worth it «split it in the tracker».",
			save, int(math.Round((1-1/ratio)*100))),
	}, true
}
