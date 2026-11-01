package dispatch

import (
	"context"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/contextstat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// agentPrompt: how the prompt Mako hands a task's agent begins (orca.Prompt).
const agentPrompt = "You are working on "

// recordContext reads how the task's agent filled its context from its Claude Code transcripts and keeps it, by
// session and phase, in place of what it read before. The agent's own session when Mako knows it; else (an
// agent in Orca or a terminal) the worktree's sessions that are the agent's, not Mako's own notes on its work.
func (d *Dispatcher) recordContext(id string) {
	r, ok := d.Store.Get(id)
	if !ok || r.Worktree == "" {
		return
	}
	paths := orca.Transcripts(r.Worktree)
	if p := orca.SessionFile(r.Worktree, r.SessionID); p != "" {
		paths = []string{p}
	}
	phase := core.PhaseWork
	if r.Planning {
		phase = core.PhasePlan
	}
	window := 0
	if s := d.Chats.Get(id); s != nil {
		window = s.Snapshot().Window
	}
	var runs []core.ContextRun
	for _, path := range paths {
		res := contextstat.AnalyzeFile(path, phase)
		if len(paths) > 1 && !agentSession(res, r.Task.ID) {
			continue
		}
		for i, run := range res.Runs {
			if run.Window = max(res.Window, window); run.Window == 0 {
				run.Window = core.ContextWindow(r.AgentModel, run.Peak)
			}
			if i == 0 && len(runs) == 0 && r.Metrics != nil && r.Metrics.Prompt > 0 {
				run.Prompt = r.Metrics.Prompt // the prompt Mako handed it, rather than a guess from the transcript
			}
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return
	}
	d.Store.Update(id, func(r *core.Record) {
		for _, run := range runs {
			r.SetContext(run)
		}
	})
}

// agentSession: a session in the task's worktree is its agent's: it began with the task's prompt, or it is no
// one-off call of Mako's (haiku, a single call) that ran there before Mako kept no transcripts of them.
func agentSession(res contextstat.Result, taskID string) bool {
	if strings.HasPrefix(strings.TrimSpace(res.First), agentPrompt) {
		return strings.Contains(res.First, taskID)
	}
	calls := 0
	for _, run := range res.Runs {
		calls += run.Calls
	}
	return calls >= 2 && !strings.Contains(strings.ToLower(res.Model), "haiku")
}

// recordContextLater reads the context in the background, once at a time per task: the turns that end
// meanwhile make it read once more, after.
func (d *Dispatcher) recordContextLater(id string) {
	d.ctxMu.Lock()
	if d.contexts == nil {
		d.contexts = map[string]bool{}
	}
	if _, busy := d.contexts[id]; busy {
		d.contexts[id] = true
		d.ctxMu.Unlock()
		return
	}
	d.contexts[id] = false
	d.ctxMu.Unlock()
	go func() {
		for {
			d.recordContext(id)
			d.ctxMu.Lock()
			if !d.contexts[id] {
				delete(d.contexts, id)
				d.ctxMu.Unlock()
				return
			}
			d.contexts[id] = false
			d.ctxMu.Unlock()
		}
	}()
}

// notePrompt keeps the size of the prompt the task's agent is started with.
func (d *Dispatcher) notePrompt(id, prompt string) {
	if prompt == "" {
		return
	}
	d.Store.Update(id, func(r *core.Record) { r.SetPrompt(prompt) })
}

// backfillContext reads the context of the tasks taken before it was kept, from the transcripts still there.
func (d *Dispatcher) backfillContext(ctx context.Context) {
	for _, r := range d.Store.All() {
		if ctx.Err() != nil {
			return
		}
		if r.StartedAt == "" || r.Worktree == "" || r.Metrics != nil && len(r.Metrics.Context) > 0 {
			continue
		}
		d.recordContext(r.ID)
	}
}
