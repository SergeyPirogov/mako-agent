package core

import (
	"maps"
	"slices"
	"time"
)

// The stages a taken task's time goes to, from the hand-off to the merge.
const (
	StageAgent  = "agent"  // the agent works
	StageYou    = "you"    // the agent waits for you: a question, a permission, its plan, its turn is over
	StageCI     = "ci"     // the MR waits for its CI
	StageReview = "review" // the MR waits for a review and the merge
)

// Stages: in the order the board shows them.
var Stages = []string{StageAgent, StageYou, StageCI, StageReview}

// The phases a task's spend goes to.
const (
	PhaseEstimate = "estimate" // the look at the task before it starts
	PhasePlan     = "plan"     // the agent in plan mode, until its plan is approved
	PhaseWork     = "work"     // the agent's work
	PhaseReview   = "review"   // the reviewer agent
	PhaseAssess   = "assess"   // the rating and the look back after
)

// Phases: in the order the board shows them.
var Phases = []string{PhaseEstimate, PhasePlan, PhaseWork, PhaseReview, PhaseAssess}

// Metrics is where a task's time went and what it cost, kept in its record as it goes, so a restart keeps them.
type Metrics struct {
	// Stage: the one the task is in now, since StageSince (RFC 3339); "" in none, and StageSince is then when the
	// last one ended.
	Stage      string `json:"stage,omitempty"`
	StageSince string `json:"stageSince,omitempty"`
	// Seconds: the time of each stage that ended, by stage.
	Seconds map[string]float64 `json:"seconds,omitempty"`
	// Cost: USD by phase; Tokens: input and output tokens by phase.
	Cost   map[string]float64 `json:"cost,omitempty"`
	Tokens map[string]int     `json:"tokens,omitempty"`
	// Context: how the agents' sessions filled their context, one run per session and phase.
	Context []ContextRun `json:"context,omitempty"`
	// Prompt: ≈ tokens of the prompt Mako handed the agent, the task's own part of its start.
	Prompt int `json:"prompt,omitempty"`
}

// clone: a copy to change: the store hands out shallow copies of a record, so its maps are never changed in place.
func (m *Metrics) clone() *Metrics {
	if m == nil {
		return &Metrics{}
	}
	c := *m
	c.Seconds, c.Cost, c.Tokens = maps.Clone(m.Seconds), maps.Clone(m.Cost), maps.Clone(m.Tokens)
	c.Context = slices.Clone(m.Context)
	return &c
}

// EnterStage ends the stage the task is in, adding its time, and starts stage at now; "" only ends it. The same
// stage again changes nothing. A task in work that is in no stage yet starts it from its hand-off (StartedAt), so
// the stages add up to the lead time: the time before the agent's first state counts too.
func (r *Record) EnterStage(stage string, now time.Time) {
	if r.Metrics == nil && stage == "" || r.Metrics != nil && r.Metrics.Stage == stage {
		return
	}
	m := r.Metrics.clone()
	r.Metrics = m
	if m.Stage != "" {
		if since, err := time.Parse(time.RFC3339Nano, m.StageSince); err == nil && now.After(since) {
			if m.Seconds == nil {
				m.Seconds = map[string]float64{}
			}
			m.Seconds[m.Stage] += now.Sub(since).Seconds()
		}
	}
	start := now
	if m.Stage == "" && stage != "" && r.Status == HandedOff {
		handed, err := time.Parse(time.RFC3339Nano, r.StartedAt)
		ended, err2 := time.Parse(time.RFC3339Nano, m.StageSince)
		if err == nil && handed.Before(now) && (err2 != nil || !handed.Before(ended)) {
			start = handed
		}
	}
	m.Stage, m.StageSince = stage, start.UTC().Format(time.RFC3339Nano)
}

// StageSeconds: the time of each stage, the one the task is in now counted up to now.
func (r *Record) StageSeconds(now time.Time) map[string]float64 {
	out := map[string]float64{}
	m := r.Metrics
	if m == nil {
		return out
	}
	for k, v := range m.Seconds {
		out[k] = v
	}
	if m.Stage != "" {
		if since, err := time.Parse(time.RFC3339Nano, m.StageSince); err == nil && now.After(since) {
			out[m.Stage] += now.Sub(since).Seconds()
		}
	}
	return out
}

// AddSpend adds what one call of an agent cost to the phase. Record.Cost stays what the task's own agent spent
// (plan and work): an agent in Orca reports nothing, and the estimate or the review of its task must not make it
// look as if it cost cents. Spent is the total.
func (r *Record) AddSpend(phase string, usd float64, tokens int) {
	if usd <= 0 && tokens <= 0 {
		return
	}
	m := r.Metrics.clone()
	r.Metrics = m
	if usd > 0 {
		if m.Cost == nil {
			m.Cost = map[string]float64{}
		}
		m.Cost[phase] += usd
		if phase == PhasePlan || phase == PhaseWork {
			r.Cost += usd
		}
	}
	if tokens > 0 {
		if m.Tokens == nil {
			m.Tokens = map[string]int{}
		}
		m.Tokens[phase] += tokens
	}
}

// TokensUsed: the tokens of all the phases.
func (r *Record) TokensUsed() int {
	if r.Metrics == nil {
		return 0
	}
	n := 0
	for _, t := range r.Metrics.Tokens {
		n += t
	}
	return n
}

// Spent: USD of all the phases: the agent's own (Cost, including what it spent before the phases were kept) and
// Mako's around it (estimate, review, assess).
func (r *Record) Spent() float64 {
	usd := r.Cost
	if r.Metrics != nil {
		for phase, c := range r.Metrics.Cost {
			if phase != PhasePlan && phase != PhaseWork {
				usd += c
			}
		}
	}
	return usd
}
