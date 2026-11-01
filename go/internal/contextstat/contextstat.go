// Package contextstat reads how an agent's context filled up from Claude Code's JSON lines: the stream of
// `claude -p --output-format stream-json` or a session's transcript, which carry the same messages. The usage of
// each model call is exact; who filled the context is the growth between two calls split between the tool
// results that came in meanwhile, by their size.
package contextstat

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// maxSteps: the most steps a run keeps; a longer one is halved, each step the larger of two.
const maxSteps = 240

// imageChars: what an image in a tool result counts as, in characters of text (≈ 1500 tokens).
const imageChars = 6000

// Result is what one stream or transcript came to.
type Result struct {
	Runs    []core.ContextRun
	Session string
	Window  int // the model's context window, when a result line told it
}

// AnalyzeFile reads a transcript; Result is empty when it cannot be read.
func AnalyzeFile(path, defaultPhase string) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{}
	}
	defer f.Close()
	return Analyze(f, defaultPhase)
}

type tool struct{ name, kind, key string }

type source struct {
	category, kind, key string
	size                int
}

type run struct {
	core.ContextRun
	files, commands map[string]*core.Heavy
}

type analyzer struct {
	runs    []*run
	cur     *run
	seen    map[string]bool
	outOf   map[string]int // a main call's output tokens, the most any of its lines told
	tools   map[string]tool
	pending []source
	lastID  string
	lastCtx int
	// compacted: a compaction came since the last call, its drop is not growth.
	compacted bool
	prompted  bool
	res       Result
}

// Analyze reads the JSON lines; the runs are in order, a plan's one (until ExitPlanMode was approved) before
// the work's; a run whose phase the lines do not tell gets defaultPhase.
func Analyze(r io.Reader, defaultPhase string) Result {
	a := &analyzer{seen: map[string]bool{}, outOf: map[string]int{}, tools: map[string]tool{}}
	a.next()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		a.line(sc.Bytes())
	}
	at := time.Now().UTC().Format(time.RFC3339)
	for _, x := range a.runs {
		if x.Calls == 0 {
			continue
		}
		if x.Phase == "" {
			x.Phase = defaultPhase
		}
		x.Session, x.At = a.res.Session, at
		x.Files, x.Commands = nonEmpty(core.Heaviest(x.files)), nonEmpty(core.Heaviest(x.commands))
		a.res.Runs = append(a.res.Runs, x.ContextRun)
	}
	return a.res
}

func (a *analyzer) next() {
	a.cur = &run{ContextRun: core.ContextRun{Tools: map[string]int{}}, files: map[string]*core.Heavy{}, commands: map[string]*core.Heavy{}}
	a.runs = append(a.runs, a.cur)
}

func (a *analyzer) line(line []byte) {
	var e struct {
		Type       string          `json:"type"`
		Subtype    string          `json:"subtype"`
		SessionID  string          `json:"session_id"`
		Session2   string          `json:"sessionId"`
		Parent     *string         `json:"parent_tool_use_id"`
		Sidechain  bool            `json:"isSidechain"`
		IsMeta     bool            `json:"isMeta"`
		Message    json.RawMessage `json:"message"`
		ModelUsage map[string]struct {
			ContextWindow int `json:"contextWindow"`
		} `json:"modelUsage"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if s := e.SessionID + e.Session2; s != "" && a.res.Session == "" {
		a.res.Session = s
	}
	main := e.Parent == nil && !e.Sidechain
	switch e.Type {
	case "system":
		if e.Subtype == "compact_boundary" {
			a.compact()
		}
	case "result":
		for _, u := range e.ModelUsage {
			a.res.Window = max(a.res.Window, u.ContextWindow)
		}
	case "assistant":
		a.assistant(e.Message, main)
	case "user":
		if main && !e.IsMeta {
			a.user(e.Message)
		}
	}
}

// compact counts a compaction once: the drop of the next call is its own.
func (a *analyzer) compact() {
	if a.compacted {
		return
	}
	a.cur.Compactions++
	a.compacted = true
}

func (a *analyzer) assistant(raw json.RawMessage, main bool) {
	var m struct {
		ID    string `json:"id"`
		Usage struct {
			Input       int `json:"input_tokens"`
			Output      int `json:"output_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	u := m.Usage
	if main {
		for _, c := range m.Content {
			if c.Type == "tool_use" {
				a.tools[c.ID] = toolOf(c.Name, c.Input)
			}
		}
	}
	if m.ID == "" || a.seen[m.ID] {
		if main && m.ID != "" && m.ID == a.lastID {
			a.outOf[m.ID] = max(a.outOf[m.ID], u.Output)
		}
		return
	}
	ctx := u.Input + u.CacheRead + u.CacheCreate
	if ctx == 0 {
		return
	}
	a.seen[m.ID] = true
	x := a.cur
	x.Input += u.Input
	x.CacheRead += u.CacheRead
	x.CacheCreate += u.CacheCreate
	if !main {
		return
	}
	if a.lastCtx > 0 && !a.compacted && ctx < a.lastCtx/2 {
		a.compact()
	}
	if a.lastCtx > 0 && !a.compacted {
		a.grow(ctx - a.lastCtx)
	}
	a.pending = a.pending[:0]
	if x.Calls == 0 {
		x.Start = ctx
	}
	x.Calls++
	x.Peak = max(x.Peak, ctx)
	if a.compacted {
		x.Compacted = append(x.Compacted, len(x.Steps))
	}
	x.Steps = append(x.Steps, ctx)
	if len(x.Steps) > maxSteps {
		x.Steps, x.Compacted = halve(x.Steps), halveMarks(x.Compacted)
	}
	a.compacted = false
	a.lastCtx, a.lastID = ctx, m.ID
	a.outOf[m.ID] = u.Output
}

// grow splits the growth since the last call: the last call's own output first, the rest between what came in
// by size.
func (a *analyzer) grow(delta int) {
	if delta <= 0 {
		return
	}
	x := a.cur
	if out := min(a.outOf[a.lastID], delta); out > 0 {
		x.Tools[core.SourceReplies] += out
		delta -= out
	}
	if delta == 0 {
		return
	}
	total := 0
	for _, s := range a.pending {
		total += s.size
	}
	if total == 0 {
		x.Tools[core.SourceOther] += delta
		return
	}
	given := 0
	for i, s := range a.pending {
		share := delta * s.size / total
		if i == len(a.pending)-1 {
			share = delta - given // the rounding goes to the last one
		}
		given += share
		x.Tools[s.category] += share
		if h := a.heavy(s); h != nil {
			h.Tokens += share
		}
	}
}

func (a *analyzer) heavy(s source) *core.Heavy {
	var m map[string]*core.Heavy
	switch s.kind {
	case "file":
		m = a.cur.files
	case "command":
		m = a.cur.commands
	default:
		return nil
	}
	if m[s.key] == nil {
		m[s.key] = &core.Heavy{Name: s.key}
	}
	return m[s.key]
}

func (a *analyzer) user(raw json.RawMessage) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		a.message(text)
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	approved := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			a.message(b.Text)
		case "tool_result":
			t, ok := a.tools[b.ToolUseID]
			if !ok {
				t = tool{name: core.SourceOther}
			}
			s := source{category: category(t.name), kind: t.kind, key: t.key, size: resultSize(b.Content)}
			if h := a.heavy(s); h != nil {
				h.Count++
			}
			a.pending = append(a.pending, s)
			if t.name == "ExitPlanMode" && !b.IsError {
				approved = true
			}
		}
	}
	if approved && a.cur.Calls > 0 {
		// The plan is approved: what follows is the work. What came in with the approval is the work's growth.
		a.cur.Phase = core.PhasePlan
		a.next()
	}
}

// message: a text of the user's side: the first one is the task's prompt, the later ones fill the context.
func (a *analyzer) message(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if !a.prompted && a.cur.Calls == 0 && !strings.HasPrefix(strings.TrimSpace(text), "<") {
		a.prompted = true
		a.cur.Prompt = (utf8.RuneCountInString(text) + 2) / 3
	}
	a.pending = append(a.pending, source{category: core.SourceMessages, size: utf8.RuneCountInString(text)})
}

// resultSize: a tool result's size in characters, an image as imageChars.
func resultSize(raw json.RawMessage) int {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return utf8.RuneCountInString(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	n := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			n += utf8.RuneCountInString(b.Text)
		case "image":
			n += imageChars
		}
	}
	return n
}

// toolOf: a tool call, with the file it read or the command it ran.
func toolOf(name string, input json.RawMessage) tool {
	var in struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	}
	json.Unmarshal(input, &in)
	t := tool{name: name}
	switch {
	case name == "Read" && in.FilePath != "":
		t.kind, t.key = "file", in.FilePath
	case name == "Bash" && strings.TrimSpace(in.Command) != "":
		line, _, _ := strings.Cut(strings.TrimSpace(in.Command), "\n")
		if r := []rune(line); len(r) > 100 {
			line = string(r[:100]) + "…"
		}
		t.kind, t.key = "command", line
	}
	return t
}

// category: what a tool counts as: an MCP server's tools as one, subagents as one.
func category(name string) string {
	switch {
	case strings.HasPrefix(name, "mcp__"):
		server, _, _ := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__")
		return "MCP: " + server
	case name == "Task" || name == "Agent":
		return core.SourceSubagents
	case name == "":
		return core.SourceOther
	}
	return name
}

func halve(steps []int) []int {
	out := make([]int, 0, (len(steps)+1)/2)
	for i := 0; i < len(steps); i += 2 {
		v := steps[i]
		if i+1 < len(steps) {
			v = max(v, steps[i+1])
		}
		out = append(out, v)
	}
	return out
}

func halveMarks(marks []int) []int {
	var out []int
	for _, m := range marks {
		if m /= 2; len(out) == 0 || out[len(out)-1] != m {
			out = append(out, m)
		}
	}
	return out
}

func nonEmpty(hs []core.Heavy) []core.Heavy {
	if len(hs) == 0 {
		return nil
	}
	return hs
}
