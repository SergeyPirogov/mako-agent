// Package chat runs Claude Code as a conversation Mako draws itself, instead of a terminal: claude -p speaks
// JSON lines both ways (stream-json), asks Mako before each tool it may not use on its own
// (--permission-prompt-tool stdio), and reports the account's usage limits.
package chat

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/proc"
)

// Item is one thing in the conversation, as the board draws it.
type Item struct {
	Kind string `json:"kind"` // "user", "text", "tool", "permission", "result", "error", "image" (Text: its link, Output: its name), "compact" (the conversation was compacted)
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
	// When it came (unix ms) and, for a tool, when it finished.
	At  int64 `json:"at,omitempty"`
	End int64 `json:"end,omitempty"`
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
	State   string `json:"state"` // "working", "waiting" (a permission), "done" (its turn ended), "limited" (the Claude limit cut it), "exited"
	Since   int64  `json:"since"`
	Live    bool   `json:"live"`
	Asleep  bool   `json:"asleep,omitempty"` // closed for being idle: a message wakes it up
	Mode    string `json:"mode"`             // how much Claude does without asking: "default", "acceptEdits", "auto", "plan", "bypassPermissions"
	Model   string `json:"model,omitempty"`
	// The context: what the last request carried, out of the model's window (tokens).
	Context int    `json:"context,omitempty"`
	Window  int    `json:"window,omitempty"`
	Items   []Item `json:"items"`
	// The items as the board draws them: turns of steps, the agent's plan, the turn going on.
	Turns    []Turn    `json:"turns"`
	Progress *Progress `json:"progress,omitempty"`
	Turn     TurnStat  `json:"turn"`
	// Plan: the plan the agent waits to have approved, in steps.
	Plan *PlanDoc `json:"plan,omitempty"`
	// LimitedUntil: when the Claude limit that cut the turn resets (unix seconds; 0: not limited, or the reset is
	// not known). ResumeOnReset: the session goes on by itself once it has.
	LimitedUntil  int64 `json:"limitedUntil,omitempty"`
	ResumeOnReset bool  `json:"resumeOnReset,omitempty"`
	// WorkMode: the mode the plan's work goes on in unless you pick another (set by Mako, not the session).
	WorkMode string `json:"workMode,omitempty"`
	// TurnUsage, TurnModel: a replayed transcript's turn going on, its tokens and the model that spent them, so
	// Mako can price it.
	TurnUsage core.Usage `json:"-"`
	TurnModel string     `json:"-"`
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
	// OnResume: the limit cut a turn and has reset, and the session was asked to go on by itself: it hands over
	// the message to say, the way your own message from the board goes (a closed agent is resumed by it). Nil: the
	// session sends it itself.
	OnResume  func(text string) error
	OnSession func(sessionID, model string) // model: the one Claude Code says it runs, "" if it did not
	// OnCost: what a turn cost (USD) and the tokens its model calls read and wrote, so the task can add up its
	// budget.
	OnCost func(usd float64, u core.Usage)
	// Gate: when set, decides each tool the agent asks for at once, nobody is asked: "" allows it, anything else
	// is why it is denied («Запитати про код»: everything but changing files). AskUserQuestion still goes to the
	// developer.
	Gate func(tool string, input json.RawMessage) string
}

// SetupResult: what a setup run before the agent printed and whether it failed.
type SetupResult struct {
	Command, Output string
	Failed          bool
}

// Session is one agent in a conversation.
type Session struct {
	ID, Worktree string

	lineMax int // maxLine as it was at Start: the session's goroutine must not read the variable later

	idleAfter time.Duration // IdleAfter as it was at Start, for the same reason

	mu       sync.Mutex
	writeMu  sync.Mutex
	cmd      *exec.Cmd
	pid      int // cmd.Process.Pid once started; Stop reads it instead of cmd.Process, which Start writes
	stdin    io.WriteCloser
	items    []Item
	version  int
	state    string
	since    time.Time
	exited   bool
	stopping bool
	asleep   bool
	idle     *time.Timer
	// The Claude limit: limitHit is told by a rate-limit event of the turn going on; limitedUntil is when the limit
	// that cut the turn resets; resumeTimer sends the message to go on then, if resumeOnReset.
	limitHit      bool
	limitedUntil  int64
	resumeOnReset bool
	resumeTimer   *time.Timer
	resumeGen     int // changes with every arming and disarming: a timer that fires late finds it is not its turn
	lastLimits    Limits
	sessionID     string
	mode          string
	model         string
	context       int
	window        int
	spent         float64                    // claude reports the cost so far of its process: what of it is already counted
	spentTok      int                        // the tokens of the process's model calls by its last result: spent / spentTok is what a token costs
	procTok       int                        // the tokens of the process's model calls so far
	lastTurn      float64                    // what the last finished turn cost (USD)
	rate          float64                    // what a token cost in the turns so far (USD), kept when a new process continues
	tokens        int                        // the tokens of this turn's model calls, told with its cost
	usage         core.Usage                 // the same tokens by kind
	calls         map[string]bool            // the model calls already counted, by message id: one comes in several events
	pending       map[string]json.RawMessage // permission request → the tool's input
	o             Options
	ended         chan struct{} // closed once the agent has exited and OnState has heard of it

	// A replayed transcript's turn going on (from your last message): its model calls' tokens and their model, for
	// what the turn costs so far when nobody runs it through Mako's chat (the agent in a terminal).
	replayUse   core.Usage
	replayModel string
}

const maxOutput = 6000

// IdleAfter: an agent that waits for you this long is closed, so it holds no memory; a message resumes it.
var IdleAfter = 20 * time.Minute

// Start runs the setup and then the agent, in the background: the conversation shows both.
func Start(o Options) *Session {
	s := &Session{ID: o.ID, Worktree: o.Worktree, state: "working", since: time.Now(), pending: map[string]json.RawMessage{}, o: o, mode: o.Mode, lineMax: maxLine, idleAfter: IdleAfter,
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
		s.add(Item{Kind: "tool", ID: "setup", Tool: "Setup", Input: mustJSON(map[string]string{"command": r.Command}), Status: "running"})
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
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.add(Item{Kind: "error", Text: "agent failed to start: " + err.Error()})
		s.end()
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		s.add(Item{Kind: "error", Text: "agent failed to start: " + err.Error()})
		s.end()
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &limitWriter{w: &stderr, n: 4000}
	s.mu.Lock()
	stopped := s.stopping
	s.cmd, s.stdin, s.spent, s.spentTok, s.procTok = cmd, stdin, 0, 0, 0
	s.mu.Unlock()
	if stopped {
		s.end()
		return
	}
	if err := cmd.Start(); err != nil {
		s.add(Item{Kind: "error", Text: "agent failed to start: " + err.Error()})
		s.end()
		return
	}
	// Stop may have come between taking the flag above and Start: it found no process to kill then.
	s.mu.Lock()
	stopped = s.stopping
	s.pid = cmd.Process.Pid
	s.mu.Unlock()
	if stopped {
		proc.KillGroup(cmd.Process.Pid)
	}
	s.write(map[string]any{"type": "control_request", "request_id": randomID(), "request": map[string]any{"subtype": "initialize"}})
	if s.o.Prompt != "" {
		s.write(userMessage(s.o.Prompt))
	} else {
		s.setState("done")
	}
	scanErr := s.scan(stdout)
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if scanErr != nil && !stopping {
		// The stream is lost (a line over the limit): nobody reads claude any more, so end it, and keep emptying
		// the pipe so it cannot block on a full one, which would hang Wait for good.
		s.add(Item{Kind: "error", Text: "agent output unreadable: " + scanErr.Error()})
		cmd.WaitDelay = 5 * time.Second
		go io.Copy(io.Discard, stdout)
		proc.KillGroup(cmd.Process.Pid)
	}
	err = cmd.Wait()
	if err != nil && !stopping && scanErr == nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		s.add(Item{Kind: "error", Text: "agent exited: " + tail(msg, 600)})
	}
	s.end()
}

// maxLine is the longest line of claude's output (a big tool result) the session reads.
var maxLine = 64 << 20

// scan hands each line of r to handle; it returns why it stopped early, nil at the end of r.
func (s *Session) scan(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), s.lineMax)
	for sc.Scan() {
		s.handle(sc.Bytes())
	}
	return sc.Err()
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

// Done is closed once the agent has exited, for a select with a timeout.
func (s *Session) Done() <-chan struct{} { return s.ended }

// replay shows a transcript: it has the same messages claude streams.
func (s *Session) replay(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if err := s.scan(f); err != nil {
		log.Printf("chat: replay of %s stopped, the history is cut: %v", path, err)
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
func Replay(id, path string) Snapshot { return ReplayIn(id, "", path) }

// ReplayIn is Replay for a conversation held in worktree: its steps name the files relative to it.
func ReplayIn(id, worktree, path string) Snapshot {
	s := &Session{ID: id, Worktree: worktree, state: "exited", exited: true, asleep: true, mode: "default", pending: map[string]json.RawMessage{}, lineMax: maxLine, idleAfter: IdleAfter}
	s.replay(path)
	snap := s.Snapshot()
	snap.Version = len(snap.Items) // the same file, the same version: the board redraws only what changed
	snap.TurnUsage, snap.TurnModel = s.replayUse, s.replayModel
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
		Timestamp  string          `json:"timestamp"` // a transcript's: when it was written
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	var at int64 // a live event: now (add sets it)
	if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
		at = t.UnixMilli()
	}
	switch e.Type {
	case "system":
		if e.Subtype == "compact_boundary" {
			// The conversation was compacted (/compact or by itself): how full the context is now, the next request says.
			s.mu.Lock()
			s.context = 0
			s.mu.Unlock()
			s.add(Item{Kind: "compact", At: at})
		}
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
					s.add(Item{Kind: "text", Text: c.Text, At: at})
				}
			case "tool_use":
				s.add(Item{Kind: "tool", ID: c.ID, Tool: c.Name, Input: c.Input, Status: "running", At: at})
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
				s.mu.Lock()
				s.replayUse = core.Usage{} // a new turn
				s.mu.Unlock()
				s.add(Item{Kind: "user", Text: t, At: at})
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
				s.finishToolAt(b.ToolUseID, tail(textOf(b.Content), maxOutput), b.IsError, at)
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
			s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": e.RequestID, "error": "Mako does not support " + r.Subtype}})
			return
		}
		if s.o.Gate != nil && r.ToolName != "AskUserQuestion" {
			resp := map[string]any{"behavior": "allow", "updatedInput": r.Input}
			if why := s.o.Gate(r.ToolName, r.Input); why != "" {
				resp = map[string]any{"behavior": "deny", "message": Refused + why}
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
	case "control_cancel_request":
		// Claude Code takes a request back (its turn was interrupted): nobody can answer it any more.
		s.cancel(e.RequestID)
	case "rate_limit_event":
		var r struct {
			Status   string `json:"status"` // "rejected" once the limit is used up
			ResetsAt int64  `json:"resetsAt"`
			Windows  map[string]struct {
				Utilization float64 `json:"utilization"`
				ResetsAt    int64   `json:"resetsAt"`
			} `json:"unifiedWindows"`
		}
		if json.Unmarshal(e.RateLimit, &r) != nil {
			break
		}
		l := Limits{At: time.Now().Unix()}
		if w, ok := r.Windows["five_hour"]; ok {
			l.FiveHour = &Window{Used: w.Utilization, ResetsAt: w.ResetsAt}
		}
		if w, ok := r.Windows["seven_day"]; ok {
			l.Week = &Window{Used: w.Utilization, ResetsAt: w.ResetsAt}
		}
		s.mu.Lock()
		if len(r.Windows) > 0 {
			s.lastLimits = l
		}
		// The turn the limit cuts ends with an error result: that is where the state changes.
		if r.Status == "rejected" {
			s.limitHit = true
			if r.ResetsAt > 0 {
				s.limitedUntil = r.ResetsAt
			}
		}
		s.mu.Unlock()
		if s.o.OnLimits != nil && len(r.Windows) > 0 {
			s.o.OnLimits(l)
		}
	case "result":
		if e.IsError && e.Result != "" {
			s.add(Item{Kind: "error", Text: e.Result, At: at})
		}
		s.mu.Lock()
		for _, u := range e.ModelUsage {
			if u.ContextWindow > s.window {
				s.window = u.ContextWindow
			}
		}
		// total_cost_usd grows over the turns of one process; a replayed transcript (no process yet) is not new.
		turn, tokens, usage := 0.0, s.tokens, s.usage
		if s.cmd != nil && e.Cost > s.spent {
			turn, s.spent = e.Cost-s.spent, e.Cost
			s.spentTok = s.procTok
			if s.spentTok > 0 {
				s.rate = s.spent / float64(s.spentTok)
			}
		}
		s.tokens, s.usage, s.lastTurn = 0, core.Usage{}, turn
		s.mu.Unlock()
		if (turn > 0 || tokens > 0) && s.o.OnCost != nil {
			s.o.OnCost(turn, usage)
		}
		// The turn is over: a request still waiting was dropped with it (an interrupt).
		s.cancel("")
		// The turn's own cost; a replayed result (no process) says only the total.
		cost := e.Cost
		if s.cmd != nil {
			cost = turn
		}
		s.add(Item{Kind: "result", Seconds: e.DurationMS / 1000, Cost: cost, At: at})
		if s.limitCut(e.IsError, e.Result) {
			s.setState("limited")
		} else {
			s.setState("done")
		}
		s.mu.Lock()
		if s.idle != nil {
			s.idle.Stop()
		}
		s.idle = time.AfterFunc(s.idleAfter, s.sleep)
		s.mu.Unlock()
	}
}

// limitText: what Claude Code says when the limit cut the turn ("You've hit your session limit · resets 3pm").
var limitText = regexp.MustCompile(`(?i)hit your .*limit`)

// limitCut tells whether the turn that just ended was cut by the Claude limit: its error result follows a
// rejected rate-limit event, or says so in words. It keeps when the limit resets (the reported window that is
// fullest, when the event did not say) and starts the automatic continuation if it was asked for.
func (s *Session) limitCut(isError bool, result string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	hit := s.limitHit
	s.limitHit = false
	if !isError || !(hit || limitText.MatchString(result)) {
		s.limitedUntil = 0
		return false
	}
	if s.limitedUntil == 0 {
		s.limitedUntil = s.lastLimits.fullest()
	}
	s.armResumeLocked()
	return true
}

// fullest: when the window that is the most used resets, 0 when none is known.
func (l Limits) fullest() int64 {
	var at int64
	used := -1.0
	for _, w := range []*Window{l.FiveHour, l.Week} {
		if w != nil && w.ResetsAt > time.Now().Unix() && w.Used > used {
			at, used = w.ResetsAt, w.Used
		}
	}
	return at
}

// ResumeGrace: how long after the limit resets the session goes on by itself.
var ResumeGrace = 30 * time.Second

// ResumeMessage: what the session says to itself to go on after the limit.
const ResumeMessage = "Continue where you stopped: the usage limit has reset."

// SetResumeOnReset makes the session go on by itself once the limit that cut its turn has reset (or takes it
// back). It works on a closed session too: the message resumes the conversation.
func (s *Session) SetResumeOnReset(on bool) {
	s.mu.Lock()
	s.resumeOnReset = on
	if on {
		s.armResumeLocked()
	} else {
		s.disarmResumeLocked()
	}
	s.version++
	s.mu.Unlock()
	s.changed()
}

// armResumeLocked (s.mu held) starts the timer that sends the continuation, if the session is limited, knows
// when the limit resets and was asked to go on.
func (s *Session) armResumeLocked() {
	s.disarmResumeLocked()
	if !s.resumeOnReset || s.limitedUntil == 0 {
		return
	}
	gen := s.resumeGen
	wait := time.Until(time.Unix(s.limitedUntil, 0).Add(ResumeGrace))
	s.resumeTimer = time.AfterFunc(max(wait, 0), func() { s.resumeNow(gen) })
}

// disarmResumeLocked (s.mu held) drops the timer: a message of yours, or Stop.
func (s *Session) disarmResumeLocked() {
	s.resumeGen++
	if s.resumeTimer != nil {
		s.resumeTimer.Stop()
		s.resumeTimer = nil
	}
}

// resumeNow is the timer's: the limit has reset, so it says "go on" the way a message from the board goes.
func (s *Session) resumeNow(gen int) {
	s.mu.Lock()
	if gen != s.resumeGen { // taken back, or a message of yours came first
		s.mu.Unlock()
		return
	}
	s.resumeTimer = nil
	s.mu.Unlock()
	send := s.o.OnResume
	if send == nil {
		send = s.Send
	}
	if err := send(ResumeMessage); err != nil {
		log.Printf("chat %s: could not go on after the limit: %v", s.ID, err)
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

func (s *Session) add(it Item) int {
	if it.At == 0 {
		it.At = time.Now().UnixMilli()
	}
	s.mu.Lock()
	s.items = append(s.items, it)
	at := len(s.items) - 1
	s.version++
	s.mu.Unlock()
	s.changed()
	return at
}

// Show puts what Mako has to show into the conversation, as a screenshot the agent took.
func (s *Session) Show(it Item) { s.add(it) }

func (s *Session) finishTool(id, output string, failed bool) { s.finishToolAt(id, output, failed, 0) }

// finishToolAt: at is when it finished (unix ms), 0 for now.
func (s *Session) finishToolAt(id, output string, failed bool, at int64) {
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	s.mu.Lock()
	for i := len(s.items) - 1; i >= 0; i-- {
		if it := &s.items[i]; it.Kind == "tool" && it.ID == id {
			it.Output, it.Status, it.End = output, "ok", at
			if failed {
				it.Status = "error"
				if strings.Contains(output, "haven't granted") || strings.Contains(output, "requires approval") || strings.Contains(output, "The developer did not allow") || strings.Contains(output, "Розробник не дозволив") /* cyrillic-ok */ || strings.Contains(output, planRevision) || strings.Contains(output, Refused) {
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

// cancel drops a request Claude Code no longer waits on ("" every one): it is marked "cancelled" and the panel
// that asked it goes away.
func (s *Session) cancel(requestID string) {
	s.mu.Lock()
	n := 0
	for i := range s.items {
		it := &s.items[i]
		if it.Kind != "permission" || it.Status != "asked" || (requestID != "" && it.ID != requestID) {
			continue
		}
		if _, ok := s.pending[it.ID]; ok {
			delete(s.pending, it.ID)
			it.Status = "cancelled"
			n++
		}
	}
	if n > 0 {
		s.version++
	}
	s.mu.Unlock()
	if n > 0 {
		s.setState("working")
		s.changed()
	}
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
		return errNotStarted
	}
	_, err := w.Write(append(b, '\n'))
	return err
}

func userMessage(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
}

// errNotStarted: the agent's process is not up yet (its history or setup is still being shown); it is no reason to stop it.
var errNotStarted = errors.New("agent has not started yet")

// ErrClosed: the agent is stopped or stopping, so it cannot take a message; resuming its conversation can.
var ErrClosed = errors.New("agent is not running")

// Send is a message from you; Claude reads it after its current step.
func (s *Session) Send(text string) error {
	s.mu.Lock()
	closed := s.exited || s.stopping // stdin is closed as soon as it stops, before the process has exited
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	// Working before Claude reads it: a turn it ends at once must not be set back to working.
	at := s.add(Item{Kind: "user", Text: text})
	// A message of yours goes on from the limit by itself: nothing is left to wait for.
	s.mu.Lock()
	wasLimited, until := s.state == "limited", s.limitedUntil
	s.limitedUntil, s.limitHit = 0, false
	s.disarmResumeLocked()
	s.mu.Unlock()
	s.setState("working")
	err := s.write(userMessage(text))
	if err == nil {
		return nil
	}
	// It never reached the agent: take it back, so it does not show as sent (and again after a resume).
	s.mu.Lock()
	s.items = append(s.items[:at], s.items[at+1:]...)
	closed = s.exited || s.stopping
	if s.state == "working" && !s.exited && len(s.pending) == 0 {
		s.state, s.since = "done", time.Now()
		if wasLimited { // still cut by the limit: the message did not go on from it
			s.state, s.limitedUntil = "limited", until
			s.armResumeLocked()
		}
	}
	s.version++
	s.mu.Unlock()
	s.changed()
	// Closed for idleness in between, or the process died: the caller resumes the conversation with the message.
	// An agent that has not started yet is neither: it is left to start.
	if closed || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return err
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
		return fmt.Errorf("this request was already answered")
	}
	resp := map[string]any{"behavior": "allow", "updatedInput": input}
	switch decision {
	case "always":
		if len(suggestions) > 0 && string(suggestions) != "null" {
			resp["updatedPermissions"] = suggestions
		}
	case "deny":
		resp = map[string]any{"behavior": "deny", "message": "The developer did not allow this."}
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
		return fmt.Errorf("this question was already answered")
	}
	if len(answers) == 0 {
		return fmt.Errorf("no answer")
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
// Claude Code's terminal does. model: the model to work with ("" keeps the one it has); notes: what you add to the
// plan, which the agent reads as your message once it goes on. Once the agent has its answer the plan is approved:
// what fails after that (the mode, the model, the notes) comes back as an ErrApprovedButNot.
func (s *Session) ApprovePlan(requestID, mode, model, notes string) error {
	notes = strings.TrimSpace(notes)
	s.mu.Lock()
	input, ok := s.pending[requestID]
	same := model == "" || model == s.model
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("this plan was already answered")
	}
	resp := map[string]any{"behavior": "allow", "updatedInput": input,
		"updatedPermissions": []map[string]any{{"type": "setMode", "mode": mode, "destination": "session"}}}
	if err := s.reply(requestID, resp, func(it *Item) { it.Status = "approved" }); err != nil {
		return err
	}
	var failed []string
	if err := s.SetMode(mode); err != nil {
		failed = append(failed, "the mode: "+err.Error())
	}
	if !same {
		if err := s.SetModel(model); err != nil {
			failed = append(failed, "the model: "+err.Error())
		}
	}
	if notes != "" {
		if err := s.Send(PlanNotesPrefix + "\n\n" + notes); err != nil {
			failed = append(failed, "the notes: "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w %s", ErrApprovedButNot, strings.Join(failed, "; "))
	}
	return nil
}

// ErrApprovedButNot: the plan is approved, but something asked with it did not reach the agent.
var ErrApprovedButNot = errors.New("the plan is approved, but the agent did not get")

// PlanNotesPrefix begins the message with your notes on an approved plan; the board shows it as the notes.
const PlanNotesPrefix = "The plan is approved. Notes from the developer to take into account while carrying it out:"

// RejectPlan turns the agent's plan down: it does not carry it out and waits for what you say next.
func (s *Session) RejectPlan(requestID string) error {
	s.mu.Lock()
	_, ok := s.pending[requestID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("this plan was already answered")
	}
	resp := map[string]any{"behavior": "deny", "message": "The developer rejected the plan: do not carry it out, change nothing; stop and wait for instructions."}
	return s.reply(requestID, resp, func(it *Item) { it.Status = "denied" })
}

// RevisePlan sends the plan back with your notes: the agent stays in plan mode and offers a new one.
func (s *Session) RevisePlan(requestID, notes string) error {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return fmt.Errorf("write what to change in the plan")
	}
	s.mu.Lock()
	_, ok := s.pending[requestID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("this plan was already answered")
	}
	resp := map[string]any{"behavior": "deny", "message": planRevision + "\n\n" + notes +
		"\n\nUpdate the plan to account for this and submit it again; change nothing until the plan is approved."}
	return s.reply(requestID, resp, func(it *Item) { it.Status, it.Text = "revised", notes })
}

const planRevision = "The developer asks to revise the plan:"

// Refused: how the reason an Options.Gate denies a tool begins.
const Refused = "Mako does not allow this here: "

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
		return fmt.Errorf("this question was already answered")
	}
	q, options, ok := Choices(*it)
	if !ok || option < 0 || option >= len(options) {
		return fmt.Errorf("there is no such answer")
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
	idle := (s.state == "done" || s.state == "limited") && !s.exited && len(s.pending) == 0
	if idle {
		s.asleep = true
		s.stopping = true // closed from here on: Send must not take a message the closing stdin will lose
	}
	s.mu.Unlock()
	if idle {
		s.stop(true) // the continuation after the limit stays: it resumes the closed conversation
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
		return fmt.Errorf("unknown mode %q", mode)
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

// SetModel changes the model the agent works with from its next request, as /model does in its terminal.
func (s *Session) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("no model")
	}
	if err := s.write(map[string]any{"type": "control_request", "request_id": randomID(), "request": map[string]any{"subtype": "set_model", "model": model}}); err != nil {
		return err
	}
	s.mu.Lock()
	s.model = model
	s.version++
	s.mu.Unlock()
	s.changed()
	return nil
}

// Model: the model the agent works with, as it said or as it was last set ("" before it says).
func (s *Session) Model() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// Interrupt stops the agent's current step, as Esc does in its terminal; the conversation goes on.
func (s *Session) Interrupt() error {
	return s.write(map[string]any{"type": "control_request", "request_id": randomID(), "request": map[string]any{"subtype": "interrupt"}})
}

// Stop ends the agent; the conversation stays in Claude Code's history (--continue).
func (s *Session) Stop() { s.stop(false) }

// stop is Stop; keepResume: a session closed for idleness still goes on after the limit.
func (s *Session) stop(keepResume bool) {
	s.mu.Lock()
	s.stopping = true
	if s.idle != nil {
		s.idle.Stop()
	}
	if !keepResume {
		s.disarmResumeLocked()
	}
	pid, w := s.pid, s.stdin
	s.mu.Unlock()
	if w != nil {
		w.Close()
	}
	if pid == 0 { // not started yet: run kills it when it sees stopping
		return
	}
	go func() {
		time.Sleep(3 * time.Second)
		if s.Running() {
			proc.KillGroup(pid)
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
	snap := Drawn(Snapshot{Version: s.version, State: s.state, Since: s.since.UnixMilli(), Live: !s.exited, Asleep: s.exited && s.asleep, Mode: s.mode, Model: s.model,
		Context: s.context, Window: s.window, Items: append([]Item{}, s.items...), LimitedUntil: s.limitedUntil, ResumeOnReset: s.resumeOnReset}, s.Worktree)
	// What the turn costs so far: its tokens at the price of the earlier turns (of this process or one before it);
	// once it ends, its result says it exactly.
	if s.state == "working" || s.state == "waiting" {
		if s.rate > 0 && s.tokens > 0 {
			snap.Turn.Cost, snap.Turn.Estimate = float64(s.tokens)*s.rate, true
		}
	} else if n := len(snap.Items); n > 0 && snap.Items[n-1].Kind == "result" {
		snap.Turn.Cost = s.lastTurn
	}
	return snap
}

// Drawn fills in what the board draws from a snapshot's items: the turns, the agent's plan, the turn going on.
// worktree: where the conversation is held, for relative paths ("" when not known).
func Drawn(snap Snapshot, worktree string) Snapshot {
	snap.Turns, snap.Progress, snap.Turn = Group(snap.Items, worktree), ProgressOf(snap.Items), statOf(snap.Items)
	// A conversation nobody runs any more: its last turn is over too.
	if n := len(snap.Turns); !snap.Live && n > 0 && snap.Turns[n-1].Who == "agent" && snap.Turns[n-1].Summary == nil {
		snap.Turns[n-1].Summary = summaryOf(snap.Turns, n-1, snap.Items, true)
	}
	for _, it := range snap.Items {
		if it.Kind == "permission" && it.Tool == PlanTool && it.Status == "asked" {
			var in struct{ Plan string }
			json.Unmarshal(it.Input, &in)
			doc := ParsePlan(in.Plan)
			snap.Plan = &doc
		}
	}
	return snap
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
func environ() []string { return proc.CleanEnv(os.Environ()) }

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

// Delete forgets the conversation: its agent stops, and the board no longer gets it.
func (m *Manager) Delete(id string) {
	m.mu.Lock()
	s := m.all[id]
	delete(m.all, id)
	m.mu.Unlock()
	if s != nil {
		s.Stop()
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
		if err := fsx.WriteFile(m.file, b, 0o600); err != nil {
			log.Printf("%s: not saved: %v", m.file, err)
		}
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
		Model string `json:"model"`
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
	u := core.Usage{Input: m.Usage.Input, Output: m.Usage.Output, CacheRead: m.Usage.CacheRead, CacheCreate: m.Usage.CacheCreate}
	n := u.Total()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n == 0 || s.calls[m.ID] {
		return
	}
	if s.calls == nil {
		s.calls = map[string]bool{}
	}
	s.calls[m.ID] = true
	if s.cmd == nil { // a replayed transcript: not new, only what its last turn spent
		s.replayUse = s.replayUse.Add(u)
		if m.Model != "" {
			s.replayModel = m.Model
		}
		return
	}
	s.tokens += n
	s.usage = s.usage.Add(u)
	s.procTok += n
}
