package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

func TestRecordContext(t *testing.T) {
	d, _, _ := setup(t)
	conf, wt := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "s-1.jsonl"), []byte(
		`{"type":"assistant","sessionId":"s-1","message":{"id":"m1","usage":{"input_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":30000}}}`+"\n"+
			`{"type":"assistant","sessionId":"s-1","message":{"id":"m2","usage":{"input_tokens":1,"cache_read_input_tokens":30000,"cache_creation_input_tokens":2000}}}`+"\n"), 0o600)
	d.Store.Update("t#1", func(r *core.Record) { r.Worktree, r.SessionID, r.AgentModel = wt, "s-1", "opus" })

	d.Store.Update("t#1", func(r *core.Record) { r.StartedAt = "2026-10-01T10:00:00Z" })
	d.backfillContext(context.Background()) // taken before the context was kept
	d.recordContext("t#1")                  // again: in place of the first
	r, _ := d.Store.Get("t#1")
	if r.Metrics == nil || len(r.Metrics.Context) != 1 {
		t.Fatalf("%+v", r.Metrics)
	}
	if c := r.Metrics.Context[0]; c.Phase != core.PhaseWork || c.Session != "s-1" || c.Start != 30001 || c.Peak != 32001 || c.Window != 200_000 {
		t.Fatalf("%+v", c)
	}

	// Mako's own calls: the estimate's context is kept, the notes on the work are not.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ncat > /dev/null\n"+
		`echo '{"type":"assistant","parent_tool_use_id":null,"message":{"id":"e1","usage":{"input_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":5000,"output_tokens":9}}}'`+"\n"+
		`echo '{"type":"result","session_id":"e-1","result":"ok","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":9}}'`+"\n"), 0o755)
	old := writer.Claude
	writer.Claude = filepath.Join(bin, "claude")
	t.Cleanup(func() { writer.Claude = old })
	for _, phase := range []string{core.PhaseEstimate, core.PhaseWork} {
		if _, err := writer.Ask(d.spending(context.Background(), "t#1", phase), wt, "?"); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = d.Store.Get("t#1")
	if len(r.Metrics.Context) != 2 || r.Metrics.Context[1].Phase != core.PhaseEstimate || r.Metrics.Context[1].Start != 5001 {
		t.Fatalf("%+v", r.Metrics.Context)
	}
}

// An agent in Orca: Mako does not know its session, the worktree has its transcript and Mako's own later
// one-off calls (kept before Mako stopped keeping them); only the agent's counts.
func TestRecordContextOfAnOrcaAgent(t *testing.T) {
	d, _, _ := setup(t)
	conf, wt := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(dir, 0o700)
	line := func(id, model string, read int, content string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","model":"` + model + `","usage":{"input_tokens":1,"cache_read_input_tokens":` +
			strconv.Itoa(read) + `,"cache_creation_input_tokens":0},"content":` + content + `}}`
	}
	write := func(name string, age time.Duration, lines ...string) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600)
		at := time.Now().Add(-age)
		os.Chtimes(p, at, at)
	}
	write("agent.jsonl", time.Hour,
		`{"type":"user","sessionId":"agent","message":{"content":"You are working on GitHub issue t#2. A developer is watching."}}`,
		line("a1", "claude-opus-5-5", 20000, `[{"type":"tool_use","id":"p","name":"ExitPlanMode","input":{}}]`),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"p","content":"ok"}]}}`,
		line("a2", "claude-opus-5-5", 25000, `[]`))
	write("mako.jsonl", time.Minute, // the summary of the work: newer, haiku
		`{"type":"user","sessionId":"mako","message":{"content":"Write what was done for the task t#2"}}`,
		line("h1", "claude-haiku-5-5", 3000, `[]`))
	d.Store.Update("t#2", func(r *core.Record) {
		r.Worktree, r.AgentModel = wt, "opus"
		r.SetPrompt(strings.Repeat("я", 300))
	})

	d.recordContext("t#2")
	r, _ := d.Store.Get("t#2")
	c := r.Metrics.Context
	if len(c) != 2 || c[0].Session != "agent" || c[0].Phase != core.PhasePlan || c[1].Phase != core.PhaseWork {
		t.Fatalf("%+v", c)
	}
	if c[0].Start != 20001 || c[1].Start != 25001 || c[0].Prompt != 100 || c[1].Prompt != 0 {
		t.Fatalf("%+v", c)
	}

	// Still planning, its own session known: one run, the plan.
	write("plan.jsonl", 0, line("q1", "claude-opus-5-5", 9000, `[]`))
	d.Store.Update("t#3", func(r *core.Record) { r.Worktree, r.SessionID, r.Planning = wt, "plan", true })
	d.recordContext("t#3")
	r, _ = d.Store.Get("t#3")
	if c := r.Metrics.Context; len(c) != 1 || c[0].Phase != core.PhasePlan || c[0].Peak != 9001 {
		t.Fatalf("%+v", c)
	}
}

// Mako does not know the session and the worktree has a single transcript, Mako's own old haiku call: it is not
// the agent's. The task is read once: the next start does not read it again.
func TestRecordContextSkipsMakosOwnLoneSession(t *testing.T) {
	d, _, _ := setup(t)
	conf, wt := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "mako.jsonl"), []byte(
		`{"type":"user","sessionId":"mako","message":{"content":"Write the title of a merge request"}}`+"\n"+
			`{"type":"assistant","message":{"id":"h1","model":"claude-haiku-5-5","usage":{"input_tokens":1,"cache_read_input_tokens":3000}}}`), 0o600)
	d.Store.Update("t#4", func(r *core.Record) { r.Worktree, r.StartedAt = wt, "2026-10-01T10:00:00Z" })

	d.backfillContext(context.Background())
	r, _ := d.Store.Get("t#4")
	if r.Metrics == nil || !r.Metrics.ContextRead || len(r.Metrics.Context) != 0 {
		t.Fatalf("%+v", r.Metrics)
	}
	// Read once: a later start leaves it alone even when a transcript turns up.
	os.WriteFile(filepath.Join(dir, "agent.jsonl"), []byte(
		`{"type":"user","sessionId":"agent","message":{"content":"You are working on GitHub issue t#4."}}`+"\n"+
			`{"type":"assistant","message":{"id":"a1","model":"claude-opus-5-5","usage":{"input_tokens":1,"cache_read_input_tokens":9000}}}`), 0o600)
	d.backfillContext(context.Background())
	if r, _ := d.Store.Get("t#4"); len(r.Metrics.Context) != 0 {
		t.Fatalf("read again: %+v", r.Metrics.Context)
	}
}

// Two sessions of the agent (a retry): the prompt Mako handed last is the newest session's, not the first's.
func TestRecordContextPromptGoesToTheNewestSession(t *testing.T) {
	d, _, _ := setup(t)
	conf, wt := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", conf)
	dir := filepath.Join(conf, "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(dir, 0o700)
	for i, name := range []string{"first", "second"} {
		p := filepath.Join(dir, name+".jsonl")
		os.WriteFile(p, []byte(`{"type":"user","sessionId":"`+name+`","message":{"content":"You are working on GitHub issue t#5."}}`+"\n"+
			`{"type":"assistant","message":{"id":"`+name+`","model":"claude-opus-5-5","usage":{"input_tokens":1,"cache_read_input_tokens":9000}}}`), 0o600)
		at := time.Now().Add(time.Duration(i-2) * time.Hour)
		os.Chtimes(p, at, at)
	}
	d.Store.Update("t#5", func(r *core.Record) { r.Worktree = wt; r.SetPrompt(strings.Repeat("я", 600)) })
	d.recordContext("t#5")
	r, _ := d.Store.Get("t#5")
	c := r.Metrics.Context
	if len(c) != 2 || c[0].Session != "first" || c[1].Session != "second" || c[1].Prompt != 200 || c[0].Prompt == 200 {
		t.Fatalf("%+v", c)
	}
}

// Turns that end while the context is read make it read once more, after; then the task is let go.
func TestRecordContextLaterCoalesces(t *testing.T) {
	d, _, _ := setup(t)
	started, release := make(chan struct{}, 10), make(chan struct{})
	var mu sync.Mutex
	reads := 0
	old := readContext
	readContext = func(_ *Dispatcher, id string) {
		mu.Lock()
		reads++
		mu.Unlock()
		started <- struct{}{}
		<-release
	}
	t.Cleanup(func() { readContext = old })

	d.recordContextLater("t#6")
	<-started
	d.recordContextLater("t#6") // meanwhile: three turns, one read more
	d.recordContextLater("t#6")
	d.recordContextLater("t#6")
	release <- struct{}{}
	<-started
	release <- struct{}{}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d.ctxMu.Lock()
		_, busy := d.contexts["t#6"]
		d.ctxMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still busy")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 {
		t.Fatalf("%d reads", reads)
	}
}
