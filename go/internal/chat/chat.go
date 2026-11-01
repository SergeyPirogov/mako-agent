// Package chat runs Claude Code as a conversation Mako draws itself, instead of a terminal: claude -p speaks
// JSON lines both ways (stream-json), asks Mako before each tool it may not use on its own
// (--permission-prompt-tool stdio), and reports the account's usage limits.
package chat

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// Item is one thing in the conversation, as the board draws it.
type Item struct {
	Kind string `json:"kind"` // "user", "text", "tool", "permission", "result", "error", "image" (Text: its link, Output: its name)
	Text string `json:"text,omitempty"`
	// A tool call and, once it ran, its result; a permission request for one.
	ID     string          `json:"id,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Status string          `json:"status,omitempty"` // tool: "running", "ok", "error", "denied"; permission: "asked", "allowed", "always", "denied"; a plan (ExitPlanMode): "approved", "revised" (Text: your notes)
	Output string          `json:"output,omitempty"`
	// Rules Claude suggests for «always allow».
	Suggestions json.RawMessage `json:"suggestions,omitempty"`
	// AskUserQuestion: what you chose, question → answer.
	Answers map[string]string `json:"answers,omitempty"`
	// The end of a turn.
	Seconds float64 `json:"seconds,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
}

// Window is one usage limit of the account: how much of it is used (0..1) and when it resets (unix seconds).
type Window struct {
	Used     float64 `json:"used"`
	ResetsAt int64   `json:"resetsAt"`
}

type Limits struct {
	FiveHour *Window `json:"fiveHour,omitempty"`
	Week     *Window `json:"week,omitempty"`
	At       int64   `json:"at"`
}

// Snapshot: the conversation and the agent's state; Version changes with every change.
type Snapshot struct {
	Version int    `json:"version"`
	State   string `json:"state"` // "working", "waiting" (a permission), "done" (its turn ended), "exited"
	Since   int64  `json:"since"`
	Live    bool   `json:"live"`
	Asleep  bool   `json:"asleep,omitempty"` // closed for being idle: a message wakes it up
	Mode    string `json:"mode"`             // how much Claude does without asking: "default", "acceptEdits", "auto", "plan", "bypassPermissions"
	Model   string `json:"model,omitempty"`
	// The context: what the last request carried, out of the model's window (tokens).
	Context int    `json:"context,omitempty"`
	Window  int    `json:"window,omitempty"`
	Items   []Item `json:"items"`
}

type Options struct {
	ID, Worktree string
	Command      string   // claude
	Args         []string // more flags: --permission-mode, --allowedTools, --continue, --settings
	Env          []string
	// SetupDone: the worktree's setup Mako ran before starting the agent ("pnpm install"), shown as finished.
	SetupDone *SetupResult
	Prompt    string // the first message; "" when the conversation continues
	Mode      string // the permission mode it starts in
	History   string // a Claude Code transcript: the conversation so far, shown before it continues
	OnState   func()
	OnLimits  func(Limits)
	OnSession func(sessionID, model string) // model: the one Claude Code says it runs, "" if it did not
	// OnCost: what a turn cost (USD) and the tokens its model calls read and wrote, so the task can add up its
	// budget.
	OnCost func(usd float64, tokens int)
	// Tools: when set, the only tools the agent may use; a request for any other is denied at once, nobody is
	// asked (the read-only «Запитати про код»: Read, Grep, Glob).
	Tools []string
}

// SetupResult: what a setup run before the agent printed and whether it failed.
type SetupResult struct {
	Command, Output string
	Failed          bool
}

// Session is one agent in a conversation.
type Session struct {
	ID, Worktree string

	mu        sync.Mutex
	writeMu   sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	items     []Item
	version   int
	state     string
	since     time.Time
	exited    bool
	stopping  bool
	asleep    bool
	idle      *time.Timer
	sessionID string
	mode      string
	model     string
	context   int
	window    int
	spent     float64                    // claude reports the cost so far of its process: what of it is already counted
	tokens    int                        // the tokens of this turn's model calls, told with its cost
	calls     map[string]bool            // the model calls already counted, by message id: one comes in several events
	pending   map[string]json.RawMessage // permission request → the tool's input
	o         Options
	ended     chan struct{} // closed once the agent has exited and OnState has heard of it
}

const maxOutput = 6000

// IdleAfter: an agent that waits for you this long is closed, so it holds no memory; a message resumes it.
var IdleAfter = 20 * time.Minute

// Start runs the setup and then the agent, in the background: the conversation shows both.
func Start(o Options) *Session {
	s := &Session{ID: o.ID, Worktree: o.Worktree, state: "working", since: time.Now(), pending: map[string]json.RawMessage{}, o: o, mode: o.Mode,
		ended: make(chan struct{})}
	if s.mode == "" {
		s.mode = "default"
	}
	if o.Prompt != "" && o.History == "" {
		s.items = append(s.items, Item{Kind: "user", Text: o.Prompt})
	}
	go s.run()
	return s
}

func (s *Session) run() {
	if s.o.History != "" {
		s.replay(s.o.History)
		if s.o.Prompt != "" {
			s.add(Item{Kind: "user", Text: s.o.Prompt}) // the message that woke it up
		}
	}
	if r := s.o.SetupDone; r != nil {
		s.add(Item{Kind: "tool", ID: "setup", Tool: "Підготовка", Input: mustJSON(map[string]string{"command": r.Command}), Status: "running"})
		s.finishTool("setup", tail(r.Output, maxOutput), r.Failed)
	}
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-prompt-tool", "stdio"}, s.o.Args...)
	if s.mode != "default" {
		args = append(args, "--permission-mode", s.mode)
	}
	cmd := exec.Command(s.o.Command, args...)
	cmd.Dir = s.Worktree
	cmd.Env = append(environ(), s.o.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &limitWriter{w: &stderr, n: 4000}
	s.mu.Lock()
	stopped := s.stopping
	s.cmd, s.stdin, s.spent = cmd, stdin, 0
	s.mu.Unlock()
	if stopped {
		s.end()
		return
	}
	if err := cmd.Start(); err != nil {
		s.add(Item{Kind: "error", Text: "агент не запустився: " + err.Error()})
		s.end()
		return
	}
	s.write(map[string]any{"type": "control_request", "request_id": randomID(), "request": map[string]any{"subtype": "initialize"}})
	if s.o.Prompt != "" {
		s.write(userMessage(s.o.Prompt))
	} else {
		s.setState("done")
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		s.handle(sc.Bytes())
	}
	err := cmd.Wait()
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if err != nil && !stopping {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		s.add(Item{Kind: "error", Text: "агент завершився: " + tail(msg, 600)})
	}
	s.end()
}

func (s *Session) end() {
	s.mu.Lock()
	s.exited = true
	s.state, s.since = "exited", time.Now()
	for i := range s.items {
		if s.items[i].Kind == "tool" && s.items[i].Status == "running" {
			s.items[i].Status = "error"
		}
	}
	s.version++
	s.mu.Unlock()
	s.changed()
	close(s.ended)
}

// Wait blocks until the agent has exited and its last change is reported.
func (s *Session) Wait() { <-s.ended }

// replay shows a transcript: it has the same messages claude streams.
func (s *Session) replay(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		s.handle(sc.Bytes())
	}
	s.mu.Lock()
	for i := range s.items { // a tool the transcript left without a result did not finish then
		if s.items[i].Kind == "tool" && s.items[i].Status == "running" {
			s.items[i].Status = "error"
		}
	}
	s.mu.Unlock()
}

// Replay: a finished conversation from its Claude Code transcript, without an agent: what the board shows of it
// until a message resumes it.
func Replay(id, path string) Snapshot {
	s := &Session{ID: id, state: "exited", exited: true, asleep: true, mode: "default", pending: map[string]json.RawMessage{}}
	s.replay(path)
	snap := s.Snapshot()
	snap.Version = len(snap.Items) // the same file, the same version: the board redraws only what changed
	return snap
}

// handle is one line from claude.
func (s *Session) handle(line []byte) {
	var e struct {
		Type       string `json:"type"`
		Subtype    string `json:"subtype"`
		SessionID  string `json:"session_id"`
		Model      string `json:"model"`
		ModelUsage map[string]struct {
			ContextWindow int `json:"contextWindow"`
		} `json:"modelUsage"`
		Parent     *string         `json:"parent_tool_use_id"`
		Message    json.RawMessage `json:"message"`
		RequestID  string          `json:"request_id"`
		Request    json.RawMessage `json:"request"`
		RateLimit  json.RawMessage `json:"rate_limit_info"`
		DurationMS float64         `json:"duration_ms"`
		Cost       float64         `json:"total_cost_usd"`
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		IsMeta     bool            `json:"isMeta"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch e.Type {
	case "system":
		if e.Subtype == "init" && e.SessionID != "" {
			s.mu.Lock()
			s.sessionID, s.model = e.SessionID, e.Model
			s.version++
			s.mu.Unlock()
			if s.o.OnSession != nil {
				s.o.OnSession(e.SessionID, e.Model)
			}
		}
	case "assistant":
		s.countTokens(e.Message)
		if e.Parent != nil { // a subagent's own steps: its Task card stands for them
			return
		}
		var m struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Usage struct {
				Input       int `json:"input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		json.Unmarshal(e.Message, &m)
		if used := m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheCreate; used > 0 {
			s.mu.Lock()
			s.context = used
			s.mu.Unlock()
		}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					s.add(Item{Kind: "text", Text: c.Text})
				}
			case "tool_use":
				s.add(Item{Kind: "tool", ID: c.ID, Tool: c.Name, Input: c.Input, Status: "running"})
				s.setState("working")
			}
		}
	case "user":
		if e.Parent != nil || e.IsMeta {
			return
		}
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		json.Unmarshal(e.Message, &m)
		// A message of yours: only a transcript has them, the stream does not repeat what Mako sent.
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			if t := strings.TrimSpace(text); t != "" && !strings.HasPrefix(t, "<") {
				s.add(Item{Kind: "user", Text: t})
			}
			return
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				s.finishTool(b.ToolUseID, tail(textOf(b.Content), maxOutput), b.IsError)
			}
		}
	case "control_request":
		var r struct {
			Subtype     string          `json:"subtype"`
			ToolName    string          `json:"tool_name"`
			Input       json.RawMessage `json:"input"`
			Suggestions json.RawMessage `json:"permission_suggestions"`
		}
		json.Unmarshal(e.Request, &r)
		if r.Subtype != "can_use_tool" {
			s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": e.RequestID, "error": "Mako не підтримує " + r.Subtype}})
			return
		}
		if len(s.o.Tools) > 0 {
			resp := map[string]any{"behavior": "allow", "updatedInput": r.Input}
			if !slices.Contains(s.o.Tools, r.ToolName) {
				resp = map[string]any{"behavior": "deny", "message": ReadOnlyDenied}
				s.add(Item{Kind: "permission", ID: e.RequestID, Tool: r.ToolName, Input: r.Input, Status: "denied"})
			}
			s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": e.RequestID, "response": resp}})
			return
		}
		s.mu.Lock()
		s.pending[e.RequestID] = r.Input
		s.mu.Unlock()
		s.add(Item{Kind: "permission", ID: e.RequestID, Tool: r.ToolName, Input: r.Input, Status: "asked", Suggestions: r.Suggestions})
		s.setState("waiting")
	case "rate_limit_event":
		var r struct {
			Windows map[string]struct {
				Utilization float64 `json:"utilization"`
				ResetsAt    int64   `json:"resetsAt"`
			} `json:"unifiedWindows"`
		}
		if json.Unmarshal(e.RateLimit, &r) == nil && s.o.OnLimits != nil {
			l := Limits{At: time.Now().Unix()}
			if w, ok := r.Windows["five_hour"]; ok {
				l.FiveHour = &Window{Used: w.Utilization, ResetsAt: w.ResetsAt}
			}
			if w, ok := r.Windows["seven_day"]; ok {
				l.Week = &Window{Used: w.Utilization, ResetsAt: w.ResetsAt}
			}
			s.o.OnLimits(l)
		}
	case "result":
		if e.IsError && e.Result != "" {
			s.add(Item{Kind: "error", Text: e.Result})
		}
		s.mu.Lock()
		for _, u := range e.ModelUsage {
			if u.ContextWindow > s.window {
				s.window = u.ContextWindow
			}
		}
		// total_cost_usd grows over the turns of one process; a replayed transcript (no process yet) is not new.
		turn, tokens := 0.0, s.tokens
		if s.cmd != nil && e.Cost > s.spent {
			turn, s.spent = e.Cost-s.spent, e.Cost
		}
		s.tokens = 0
		s.mu.Unlock()
		if (turn > 0 || tokens > 0) && s.o.OnCost != nil {
			s.o.OnCost(turn, tokens)
		}
		s.add(Item{Kind: "result", Seconds: e.DurationMS / 1000, Cost: e.Cost})
		s.setState("done")
		s.mu.Lock()
		if s.idle != nil {
			s.idle.Stop()
		}
		s.idle = time.AfterFunc(IdleAfter, s.sleep)
		s.mu.Unlock()
	}
}

func textOf(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (s *Session) add(it Item) {
	s.mu.Lock()
	s.items = append(s.items, it)
	s.version++
	s.mu.Unlock()
	s.changed()
}

// Show puts what Mako has to show into the conversation, as a screenshot the agent took.
func (s *Session) Show(it Item) { s.add(it) }

func (s *Session) finishTool(id, output string, failed bool) {
	s.mu.Lock()
	for i := len(s.items) - 1; i >= 0; i-- {
		if it := &s.items[i]; it.Kind == "tool" && it.ID == id {
			it.Output, it.Status = output, "ok"
			if failed {
				it.Status = "error"
				if strings.Contains(output, "haven't granted") || strings.Contains(output, "requires approval") || strings.Contains(output, "Розробник не дозволив") || strings.Contains(output, planRevision) || strings.Contains(output, ReadOnlyDenied) {
					it.Status = "denied"
				}
			}
			break
		}
	}
	s.version++
	s.mu.Unlock()
	s.changed()
}

func (s *Session) setState(st string) {
	s.mu.Lock()
	if s.exited || s.state == st {
		s.mu.Unlock()
		return
	}
	if st != "waiting" && len(s.pending) > 0 { // still a permission to answer
		s.mu.Unlock()
		return
	}
	s.state, s.since = st, time.Now()
	s.version++
	s.mu.Unlock()
	s.changed()
}

func (s *Session) changed() {
	if s.o.OnState != nil {
		s.o.OnState()
	}
}

func (s *Session) write(v any) error {
	b, _ := json.Marshal(v)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w == nil {
		return fmt.Errorf("агент ще не запустився")
	}
	_, err := w.Write(append(b, '\n'))
	return err
}

func userMessage(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
}

// Send is a message from you; Claude reads it after its current step.
func (s *Session) Send(text string) error {
	if !s.Running() {
		return fmt.Errorf("агент не запущений")
	}
	// Working before Claude reads it: a turn it ends at once must not be set back to working.
	s.add(Item{Kind: "user", Text: text})
	s.setState("working")
	return s.write(userMessage(text))
}

// Say is Send for the board's ✨ requests.
func (s *Session) Say(text string) bool { return s.Send(text) == nil }

// Permit answers a permission request: "allow", "always" (allow and keep Claude's suggested rule), "deny".
func (s *Session) Permit(requestID, decision string) error {
	s.mu.Lock()
	input, ok := s.pending[requestID]
	var suggestions json.RawMessage
	for i := range s.items {
		if s.items[i].Kind == "permission" && s.items[i].ID == requestID {
			suggestions = s.items[i].Suggestions
		}
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("на цей запит уже відповіли")
	}
	resp := map[string]any{"behavior": "allow", "updatedInput": input}
	switch decision {
	case "always":
		if len(suggestions) > 0 && string(suggestions) != "null" {
			resp["updatedPermissions"] = suggestions
		}
	case "deny":
		resp = map[string]any{"behavior": "deny", "message": "Розробник не дозволив це зробити."}
	}
	return s.reply(requestID, resp, func(it *Item) {
		it.Status = map[string]string{"allow": "allowed", "always": "always", "deny": "denied"}[decision]
	})
}

// Answer replies to the agent's questions (AskUserQuestion): question → the option chosen or your own words.
func (s *Session) Answer(requestID string, answers map[string]string) error {
	s.mu.Lock()
	raw, ok := s.pending[requestID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("на це питання вже відповіли")
	}
	if len(answers) == 0 {
		return fmt.Errorf("немає відповіді")
	}
	input := map[string]any{}
	json.Unmarshal(raw, &input)
	input["answers"] = answers
	resp := map[string]any{"behavior": "allow", "updatedInput": input}
	return s.reply(requestID, resp, func(it *Item) { it.Status, it.Answers = "answered", answers })
}

// PlanTool is how Claude Code in plan mode hands its plan over: a request to leave plan mode with the plan.
const PlanTool = "ExitPlanMode"

// ApprovePlan accepts the agent's plan (ExitPlanMode) and lets it work in mode, as approving a plan in
// Claude Code's terminal does.
func (s *Session) ApprovePlan(requestID, mode string) error {
	s.mu.Lock()
	input, ok := s.pending[requestID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("на цей план уже відповіли")
	}
	resp := map[string]any{"behavior": "allow", "updatedInput": input,
		"updatedPermissions": []map[string]any{{"type": "setMode", "mode": mode, "destination": "session"}}}
	if err := s.reply(requestID, resp, func(it *Item) { it.Status = "approved" }); err != nil {
		return err
	}
	return s.SetMode(mode)
}

// RevisePlan sends the plan back with your notes: the agent stays in plan mode and offers a new one.
func (s *Session) RevisePlan(requestID, notes string) error {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return fmt.Errorf("напишіть, що поправити в плані")
	}
	s.mu.Lock()
	_, ok := s.pending[requestID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("на цей план уже відповіли")
	}
	resp := map[string]any{"behavior": "deny", "message": planRevision + "\n\n" + notes +
		"\n\nОнови план з урахуванням цього й подай його знову; нічого не змінюй, доки план не затверджено."}
	return s.reply(requestID, resp, func(it *Item) { it.Status, it.Text = "revised", notes })
}

const planRevision = "Розробник просить поправити план:"

// ReadOnlyDenied: what an agent limited to Options.Tools hears when it asks for another tool.
const ReadOnlyDenied = "Тут можна лише читати код (Read, Grep, Glob): нічого не змінюй і не запускай."

// Asked: the request the agent waits on (a permission or AskUserQuestion), nil when there is none.
func (s *Session) Asked() *Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if it := s.items[i]; it.Kind == "permission" && it.Status == "asked" {
			if _, ok := s.pending[it.ID]; ok {
				return &it
			}
		}
	}
	return nil
}

// Choices: the question and its options when an AskUserQuestion asks exactly one question with one answer, the
// kind a single tap answers; ok is false for anything else.
func Choices(it Item) (question string, options []string, ok bool) {
	if it.Tool != "AskUserQuestion" {
		return "", nil, false
	}
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(it.Input, &in) != nil || len(in.Questions) != 1 || in.Questions[0].MultiSelect {
		return "", nil, false
	}
	q := in.Questions[0]
	for _, o := range q.Options {
		options = append(options, o.Label)
	}
	return q.Question, options, q.Question != "" && len(options) > 0
}

// Choose answers a one-question AskUserQuestion with its option number option (from 0).
func (s *Session) Choose(requestID string, option int) error {
	it := s.Asked()
	if it == nil || it.ID != requestID {
		return fmt.Errorf("на це питання вже відповіли")
	}
	q, options, ok := Choices(*it)
	if !ok || option < 0 || option >= len(options) {
		return fmt.Errorf("такої відповіді немає")
	}
	return s.Answer(requestID, map[string]string{q: options[option]})
}

// reply sends Claude the answer to a request. The request is marked answered and the session working before
// Claude reads the answer: a turn it then ends at once would otherwise be set back to working. If the answer
// cannot be sent, the request is asked again.
func (s *Session) reply(requestID string, resp map[string]any, mark func(*Item)) error {
	s.mu.Lock()
	raw := s.pending[requestID]
	delete(s.pending, requestID)
	var before []Item
	for i := range s.items {
		if s.items[i].Kind == "permission" && s.items[i].ID == requestID {
			before = append(before, s.items[i])
			mark(&s.items[i])
		}
	}
	s.version++
	s.mu.Unlock()
	s.setState("working")
	if err := s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": resp}}); err != nil {
		s.mu.Lock()
		s.pending[requestID] = raw
		for i, k := 0, 0; i < len(s.items) && k < len(before); i++ {
			if s.items[i].Kind == "permission" && s.items[i].ID == requestID {
				s.items[i], k = before[k], k+1
			}
		}
		s.version++
		s.mu.Unlock()
		s.setState("waiting")
		return err
	}
	return nil
}

// sleep closes an agent that has waited for you IdleAfter; its conversation stays, and a message resumes it.
func (s *Session) sleep() {
	s.mu.Lock()
	idle := s.state == "done" && !s.exited && len(s.pending) == 0
	if idle {
		s.asleep = true
	}
	s.mu.Unlock()
	if idle {
		s.Stop()
	}
}

// Modes Claude Code knows, as the board offers them.
var Modes = []string{"default", "acceptEdits", "auto", "plan", "bypassPermissions"}

// SetMode changes how much Claude does without asking, at once, as Shift+Tab does in its terminal.
func (s *Session) SetMode(mode string) error {
	ok := false
	for _, m := range Modes {
		ok = ok || m == mode
	}
	if !ok {
		return fmt.Errorf("невідомий режим %q", mode)
	}
	if err := s.write(map[string]any{"type": "control_request", "request_id": randomID(), "request": map[string]any{"subtype": "set_permission_mode", "mode": mode}}); err != nil {
		return err
	}
	s.mu.Lock()
	s.mode = mode
	s.version++
	s.mu.Unlock()
	s.changed()
	return nil
}

// Interrupt stops the agent's current step, as Esc does in its terminal; the conversation goes on.
func (s *Session) Interrupt() error {
	return s.write(map[string]any{"type": "control_request", "request_id": randomID(), "request": map[string]any{"subtype": "interrupt"}})
}

// Stop ends the agent; the conversation stays in Claude Code's history (--continue).
func (s *Session) Stop() {
	s.mu.Lock()
	s.stopping = true
	if s.idle != nil {
		s.idle.Stop()
	}
	cmd, w := s.cmd, s.stdin
	s.mu.Unlock()
	if w != nil {
		w.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	go func() {
		time.Sleep(3 * time.Second)
		if s.Running() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
}

func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exited
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{Version: s.version, State: s.state, Since: s.since.UnixMilli(), Live: !s.exited, Asleep: s.exited && s.asleep, Mode: s.mode, Model: s.model,
		Context: s.context, Window: s.window, Items: append([]Item{}, s.items...)}
}

// State: what the agent does now and the tool it runs.
func (s *Session) State() (state, tool string, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.items) - 1; i >= 0; i-- {
		if it := s.items[i]; it.Kind == "tool" && it.Status == "running" {
			tool = it.Tool + toolArg(it.Input)
			break
		}
		if it := s.items[i]; it.Kind == "permission" && it.Status == "asked" {
			tool = it.Tool + toolArg(it.Input)
			break
		}
	}
	return s.state, tool, s.since
}

// LastText: the agent's last message.
func (s *Session) LastText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.items) - 1; i >= 0; i-- {
		if s.items[i].Kind == "text" {
			return s.items[i].Text
		}
	}
	return ""
}

func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func toolArg(input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "pattern", "url", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			v = strings.TrimSpace(strings.SplitN(v, "\n", 2)[0])
			if r := []rune(v); len(r) > 120 {
				v = string(r[:119]) + "…"
			}
			return ": " + v
		}
	}
	return ""
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// environ: the environment without the marks of a Claude Code session Mako may run in.
func environ() []string { return term.CleanEnv(os.Environ()) }

// Manager keeps the conversations of all tasks and the account's last known limits; the limits are kept in a
// file too, so a restarted Mako shows them before an agent reports again.
type Manager struct {
	mu     sync.Mutex
	all    map[string]*Session
	limits Limits
	file   string
}

func NewManager(limitsFile string) *Manager {
	m := &Manager{all: map[string]*Session{}, file: limitsFile}
	if b, err := os.ReadFile(limitsFile); err == nil {
		json.Unmarshal(b, &m.limits)
	}
	return m
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all[id]
}

func (m *Manager) Put(s *Session) {
	m.mu.Lock()
	old := m.all[s.ID]
	m.all[s.ID] = s
	m.mu.Unlock()
	if old != nil {
		old.Stop()
	}
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.all {
		s.Stop()
	}
}

// Busy: how many agents are in the middle of a turn (Mako's restart waits for them).
func (m *Manager) Busy() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.all {
		if st, _, _ := s.State(); st == "working" && s.Running() {
			n++
		}
	}
	return n
}

// Live: how many agents still run.
func (m *Manager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.all {
		if s.Running() {
			n++
		}
	}
	return n
}

func (m *Manager) SetLimits(l Limits) {
	m.mu.Lock()
	m.limits = l
	m.mu.Unlock()
	if m.file != "" {
		b, _ := json.Marshal(l)
		os.WriteFile(m.file, b, 0o644)
	}
}

func (m *Manager) Limits() Limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits
}

// countTokens adds the tokens of a model call of the running process (the agent's or a subagent's) to the turn's,
// once per call.
func (s *Session) countTokens(message json.RawMessage) {
	var m struct {
		ID    string `json:"id"`
		Usage struct {
			Input       int `json:"input_tokens"`
			Output      int `json:"output_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(message, &m) != nil || m.ID == "" {
		return
	}
	u := m.Usage
	n := u.Input + u.Output + u.CacheRead + u.CacheCreate
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || n == 0 || s.calls[m.ID] { // a replayed transcript is not new
		return
	}
	if s.calls == nil {
		s.calls = map[string]bool{}
	}
	s.calls[m.ID] = true
	s.tokens += n
}
