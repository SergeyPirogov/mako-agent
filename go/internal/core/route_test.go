package core

import (
	"strings"
	"testing"
)

// A level up raises the effort to xhigh, then the model; on opus the effort goes on to max. A level down lowers
// the effort to low, then the model.
func TestStep(t *testing.T) {
	r := func(m, e string) Route { return Route{Model: m, Effort: e} }
	cases := []struct {
		from Route
		n    int
		want Route
	}{
		{r("sonnet", "medium"), 1, r("sonnet", "high")},
		{r("sonnet", "high"), 2, r("opus", "xhigh")},
		{r("sonnet", "xhigh"), 1, r("opus", "xhigh")},
		{r("opus", "xhigh"), 1, r("opus", "max")},
		{r("opus", "max"), 3, r("opus", "max")},
		{r("haiku", "medium"), -1, r("haiku", "low")},
		{r("sonnet", "low"), -1, r("haiku", "low")},
		{r("haiku", "low"), -2, r("haiku", "low")},
		{r("opus", "high"), 0, r("opus", "high")},
		{r("claude-opus-5-5", "high"), 2, r("claude-opus-5-5", "max")},
	}
	for _, c := range cases {
		if got := Step(c.from, c.n); got != c.want {
			t.Errorf("Step(%v, %d) = %v, want %v", c.from, c.n, got, c.want)
		}
	}
}

func TestDefaultRoutes(t *testing.T) {
	d := DefaultRoutes()
	for _, p := range RoutePhases {
		for _, s := range Sizes {
			if d[p][s].Model == "" || d[p][s].Effort == "" {
				t.Errorf("%s/%s: %+v", p, s, d[p][s])
			}
		}
	}
	if d[PhaseWork]["big"] != (Route{Model: "opus", Effort: "high"}) || d[PhaseEstimate]["big"] != (Route{Model: "haiku", Effort: "medium"}) {
		t.Errorf("table %+v", d)
	}
	if (Route{Model: "opus", Effort: "high"}).Label() != "opus · high" {
		t.Error("label")
	}
}

// LabelWithMode names the mode only when it moved the route; balanced, or none, adds nothing.
func TestRouteLabelWithMode(t *testing.T) {
	for _, c := range []struct {
		r    Route
		want string
	}{
		{Route{Model: "opus", Effort: "high", Mode: ModeQuality}, "opus · high (quality)"},
		{Route{Model: "haiku", Effort: "low", Mode: ModeCost}, "haiku · low (cost)"},
		{Route{Model: "opus", Effort: "high", Mode: ModeBalanced}, "opus · high"},
		{Route{Model: "opus", Effort: "high"}, "opus · high"},
		{Route{Model: "opus", Mode: ModeQuality}, "opus (quality)"},
		{Route{Mode: ModeQuality}, ""},
		{Route{Model: "opus", Effort: "high", Mode: "turbo"}, "opus · high"},
	} {
		if got := c.r.LabelWithMode(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
		if got := c.r.Label(); strings.Contains(got, "(") {
			t.Errorf("Label of %+v names the mode: %q", c.r, got)
		}
	}
}

// The routes are kept in the metrics, and a copy of the record does not share them.
func TestSetRoute(t *testing.T) {
	var r Record
	r.SetRoute(PhaseWork, Route{Model: "opus", Effort: "high"})
	c := r
	c.SetRoute(PhaseWork, Route{Model: "sonnet"})
	if r.RouteOf(PhaseWork).Model != "opus" || c.RouteOf(PhaseWork).Model != "sonnet" {
		t.Errorf("%+v %+v", r.Metrics.Routes, c.Metrics.Routes)
	}
}
