package core

import (
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// ContextRun is how one agent session filled its context in one phase: from the usage of each model call
// (exact) and the growth between the calls, split between what came in meanwhile (approximate).
type ContextRun struct {
	Session string `json:"session,omitempty"`
	Phase   string `json:"phase"`
	At      string `json:"at,omitempty"` // when it was last counted, RFC 3339
	// Window: the model's context window; Start: the context of the first call (system prompt, tools, MCP,
	// CLAUDE.md, memory and the prompt); Prompt: ≈ tokens of the prompt alone; Peak: the largest context.
	Window      int `json:"window,omitempty"`
	Start       int `json:"start"`
	Prompt      int `json:"prompt,omitempty"`
	Peak        int `json:"peak"`
	Calls       int `json:"calls"`
	Compactions int `json:"compactions,omitempty"`
	// Input, CacheRead, CacheCreate: the input tokens of all calls, subagents' too (in a transcript, from their
	// own files next to it).
	Input       int `json:"input"`
	CacheRead   int `json:"cacheRead"`
	CacheCreate int `json:"cacheCreate"`
	// Tools: the context's growth by what brought it: a tool, "MCP: <server>", Subagents, Replies, Messages, Other.
	Tools    map[string]int `json:"tools,omitempty"`
	Files    []Heavy        `json:"files,omitempty"`    // the heaviest files read, by tokens
	Commands []Heavy        `json:"commands,omitempty"` // the commands with the largest output
	// Steps: the context of each call (downsampled, each the largest of the calls it stands for); Compacted: the
	// steps right after a compaction.
	Steps     []int `json:"steps,omitempty"`
	Compacted []int `json:"compacted,omitempty"`
}

// Heavy is a file or a command and what it brought into the context.
type Heavy struct {
	Name   string `json:"name"`
	Count  int    `json:"count"`
	Tokens int    `json:"tokens"`
}

// The growth sources that are not tools.
const (
	SourceReplies   = "Replies"
	SourceMessages  = "Messages"
	SourceOther     = "Other"
	SourceSubagents = "Subagents"
)

// TopHeavy: the most a run (and the statistics) keep of the files and the commands.
const TopHeavy = 10

// maxContextRuns: the runs a record keeps, the newest.
const maxContextRuns = 30

// SetContext keeps run: in place of the one of the same session and phase, else added; past maxContextRuns the
// oldest of Mako's own runs go first.
func (r *Record) SetContext(run ContextRun) {
	m := r.Metrics.clone()
	r.Metrics = m
	for i, old := range m.Context {
		if old.Session != "" && old.Session == run.Session && old.Phase == run.Phase {
			m.Context[i] = run
			return
		}
	}
	m.Context = append(m.Context, run)
	for len(m.Context) > maxContextRuns {
		m.Context = slices.Delete(m.Context, oldestToDrop(m.Context), oldestToDrop(m.Context)+1)
	}
}

// oldestToDrop: the oldest run of Mako's own (an estimate, a review, an assessment: each its own session), so the
// agent's plan and work, which the card's line and the start stand on, stay; the oldest of all when there is none.
func oldestToDrop(runs []ContextRun) int {
	for i, x := range runs {
		if x.Phase != PhasePlan && x.Phase != PhaseWork {
			return i
		}
	}
	return 0
}

// ContextWasRead marks that the transcripts were read for Context.
func (r *Record) ContextWasRead() {
	if r.Metrics != nil && r.Metrics.ContextRead {
		return
	}
	m := r.Metrics.clone()
	r.Metrics = m
	m.ContextRead = true
}

// SetPrompt keeps the size of the prompt Mako handed the agent.
func (r *Record) SetPrompt(text string) {
	m := r.Metrics.clone()
	r.Metrics = m
	m.Prompt = PromptTokens(text)
}

// PromptTokens: ≈ the tokens of a prompt, from its length: Mako knows the text, not how the model counts it.
func PromptTokens(text string) int { return (utf8.RuneCountInString(text) + 2) / 3 }

// ContextWindow: the model's window when the stream did not tell it: 1M for a "[1m]" model or a context that
// grew past 200k, else 200k.
func ContextWindow(model string, peak int) int {
	if strings.Contains(strings.ToLower(model), "[1m]") || peak > 200_000 {
		return 1_000_000
	}
	return 200_000
}

// ContextStats is how the agents' context filled over the period, by phase.
type ContextStats struct {
	Phases map[string]*ContextPhase `json:"phases"`
	// Files, Commands: the heaviest over the period, their tasks' tops added up.
	Files    []Heavy `json:"files"`
	Commands []Heavy `json:"commands"`
}

// ContextPhase: the runs of one phase. Start, Prompt, Peak, PeakShare: medians over the runs.
type ContextPhase struct {
	Runs        int            `json:"runs"`
	Start       float64        `json:"start"`
	Prompt      float64        `json:"prompt"`
	Peak        float64        `json:"peak"`
	PeakShare   float64        `json:"peakShare"` // of the model's window, 0..1
	MaxPeak     int            `json:"maxPeak"`
	Compactions int            `json:"compactions"`
	Compacted   int            `json:"compacted"` // the runs compacted at least once
	Input       int            `json:"input"`
	CacheRead   int            `json:"cacheRead"`
	CacheCreate int            `json:"cacheCreate"`
	Tools       map[string]int `json:"tools"`

	starts, prompts, peaks, shares []float64
}

// contextStats adds up the context runs of the records.
type contextStats struct {
	st              ContextStats
	files, commands map[string]*Heavy
}

func newContextStats() *contextStats {
	return &contextStats{st: ContextStats{Phases: map[string]*ContextPhase{}, Files: []Heavy{}, Commands: []Heavy{}},
		files: map[string]*Heavy{}, commands: map[string]*Heavy{}}
}

func (c *contextStats) add(r Record) {
	if r.Metrics == nil {
		return
	}
	for _, x := range r.Metrics.Context {
		p := c.st.Phases[x.Phase]
		if p == nil {
			p = &ContextPhase{Tools: map[string]int{}}
			c.st.Phases[x.Phase] = p
		}
		p.Runs++
		p.starts = append(p.starts, float64(x.Start))
		if x.Prompt > 0 {
			p.prompts = append(p.prompts, float64(x.Prompt))
		}
		p.peaks = append(p.peaks, float64(x.Peak))
		if x.Window > 0 {
			p.shares = append(p.shares, float64(x.Peak)/float64(x.Window))
		}
		p.MaxPeak = max(p.MaxPeak, x.Peak)
		p.Compactions += x.Compactions
		if x.Compactions > 0 {
			p.Compacted++
		}
		p.Input += x.Input
		p.CacheRead += x.CacheRead
		p.CacheCreate += x.CacheCreate
		for k, v := range x.Tools {
			p.Tools[k] += v
		}
		addHeavy(c.files, x.Files)
		addHeavy(c.commands, x.Commands)
	}
}

func addHeavy(m map[string]*Heavy, hs []Heavy) {
	for _, h := range hs {
		if m[h.Name] == nil {
			m[h.Name] = &Heavy{Name: h.Name}
		}
		m[h.Name].Count += h.Count
		m[h.Name].Tokens += h.Tokens
	}
}

func (c *contextStats) done() ContextStats {
	for _, p := range c.st.Phases {
		p.Start, p.Prompt, p.Peak, p.PeakShare = median(p.starts), median(p.prompts), median(p.peaks), median(p.shares)
	}
	c.st.Files, c.st.Commands = Heaviest(c.files), Heaviest(c.commands)
	return c.st
}

// Heaviest: by tokens, then by count, at most TopHeavy.
func Heaviest(m map[string]*Heavy) []Heavy {
	out := make([]Heavy, 0, len(m))
	for _, h := range m {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Name < b.Name
	})
	if len(out) > TopHeavy {
		out = out[:TopHeavy]
	}
	return out
}
