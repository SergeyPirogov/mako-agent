// Package runner runs Claude Code for a task itself: a background session in the task's worktree, spoken to
// over stream-json. The developer reads it, writes to it and answers its permission requests and questions
// from Mako, on the Mac or the phone.
package runner

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// Options: how the agent runs; read at every start, so the settings apply to the next session.
type Options struct {
	Command        string   // "claude" by default
	Model          string   // "" — Claude Code's own default
	PermissionMode string   // "default", "acceptEdits", "auto", "bypassPermissions"
	AllowedTools   []string // rules allowed without asking: "Bash(git push:*)"
}

// Request is something the agent waits for the developer on: a permission for a tool, or its questions.
type Request struct {
	ID          string          `json:"id"`
	Tool        string          `json:"tool"`
	What        string          `json:"what"` // «Bash: npm test»
	Description string          `json:"description,omitempty"`
	Questions   []Question      `json:"questions,omitempty"` // AskUserQuestion
	At          int64           `json:"at"`
	input       json.RawMessage `json:"-"`
	suggestions json.RawMessage `json:"-"`
}

type Question struct {
	Question string   `json:"question"`
	Header   string   `json:"header,omitempty"`
	Options  []string `json:"options"`
	Multi    bool     `json:"multiSelect,omitempty"`
}

// Status of a task's agent.
type Status struct {
	Running   bool      `json:"running"` // the Claude Code process is alive
	Busy      bool      `json:"busy"`    // a turn is under way
	Requests  []Request `json:"requests,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	LastText  string    `json:"lastText,omitempty"`
	Since     int64     `json:"since"`
	Error     string    `json:"error,omitempty"`
	SessionID string    `json:"sessionId"`
}

type session struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	st      Status
	pending map[string]*Request
	stopped bool
	idle    *time.Timer
}

// IdleAfter: a session without a turn this long is closed; the next message resumes it.
var IdleAfter = 20 * time.Minute

type Runner struct {
	Opts     func() Options
	OnChange func(id string) // a turn started or ended, a request came or went
	Log      func(id, text string)

	mu       sync.Mutex
	sessions map[string]*session
}

func New(opts func() Options) *Runner {
	return &Runner{Opts: opts, sessions: map[string]*session{}, OnChange: func(string) {}, Log: func(string, string) {}}
}

// NewSessionID: a UUID for Claude Code's --session-id.
func NewSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Start begins a new session in dir with the task's prompt.
func (r *Runner) Start(id, dir, sessionID, prompt string) error {
	if err := r.spawn(id, dir, sessionID, false); err != nil {
		return err
	}
	return r.write(id, userMessage(prompt))
}

// Send gives the agent a message: into the running session, or by resuming it (after a restart of Mako, or
// when it was closed for being idle). Sent during a turn, Claude Code takes it when it can.
func (r *Runner) Send(id, dir, sessionID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("порожнє повідомлення")
	}
	if !r.alive(id) {
		if sessionID == "" {
			return errors.New("у задачі немає сесії агента")
		}
		if err := r.spawn(id, dir, sessionID, true); err != nil {
			return err
		}
	}
	return r.write(id, userMessage(text))
}

// Answer replies to a request: allow (always: and remember the rule for this worktree) or deny with a reason;
// answers carry the choices for AskUserQuestion.
func (r *Runner) Answer(id, reqID string, allow, always bool, message string, answers map[string]string) error {
	s := r.get(id)
	if s == nil {
		return errors.New("агент не запущений")
	}
	s.mu.Lock()
	req := s.pending[reqID]
	delete(s.pending, reqID)
	s.refreshLocked()
	s.mu.Unlock()
	if req == nil {
		return errors.New("цей запит уже неактуальний")
	}
	var resp map[string]any
	if allow {
		input := map[string]any{}
		json.Unmarshal(req.input, &input)
		if len(answers) > 0 {
			input["answers"] = answers
		}
		resp = map[string]any{"behavior": "allow", "updatedInput": input}
		if always && len(req.suggestions) > 0 {
			resp["updatedPermissions"] = req.suggestions
		}
	} else {
		if message == "" {
			message = "Розробник не дозволив."
		}
		resp = map[string]any{"behavior": "deny", "message": message}
	}
	r.OnChange(id)
	return r.write(id, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": reqID, "response": resp}})
}

// Interrupt stops the current turn; the session stays for the next message.
func (r *Runner) Interrupt(id string) error {
	return r.write(id, map[string]any{"type": "control_request", "request_id": NewSessionID(), "request": map[string]any{"subtype": "interrupt"}})
}

// Stop ends the session's process (the conversation stays in Claude Code's transcript).
func (r *Runner) Stop(id string) {
	s := r.get(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// Status of the task's agent; false when Mako has not run it since it started.
func (r *Runner) Status(id string) (Status, bool) {
	s := r.get(id)
	if s == nil {
		return Status{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Requests = append([]Request{}, s.st.Requests...)
	return st, true
}

func (r *Runner) get(id string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *Runner) alive(id string) bool {
	s := r.get(id)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Running
}

func userMessage(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
}

func (r *Runner) write(id string, v any) error {
	s := r.get(id)
	if s == nil {
		return errors.New("агент не запущений")
	}
	line, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.st.Running || s.stdin == nil {
		return errors.New("агент не запущений")
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("агенту не передано: %w", err)
	}
	if m, _ := v.(map[string]any); m["type"] == "user" {
		s.st.Busy, s.st.Since, s.st.Error = true, time.Now().UnixMilli(), ""
	}
	return nil
}

// spawn starts Claude Code in dir: a new session, or resuming one.
func (r *Runner) spawn(id, dir, sessionID string, resume bool) error {
	o := r.Opts()
	command := o.Command
	if command == "" {
		command = "claude"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return errors.New("Claude Code не знайдено: встановіть його (claude.com/claude-code) або вкажіть команду в налаштуваннях")
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-prompt-tool", "stdio"}
	if resume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(o.AllowedTools, ","))
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Claude Code не запустився: %w", err)
	}
	s := &session{cmd: cmd, stdin: stdin, pending: map[string]*Request{}}
	s.st = Status{Running: true, SessionID: sessionID, Since: time.Now().UnixMilli()}
	r.mu.Lock()
	r.sessions[id] = s
	r.mu.Unlock()
	if resume {
		r.Log(id, "агент продовжує сесію")
	} else {
		r.Log(id, "агента запущено")
	}
	go func() {
		r.read(id, s, stdout)
		err := cmd.Wait()
		s.mu.Lock()
		s.st.Running, s.st.Busy, s.st.Tool, s.st.Requests = false, false, "", nil
		s.pending = map[string]*Request{}
		if err != nil && !s.stopped {
			s.st.Error = strings.TrimSpace("Claude Code завершився з помилкою: " + stderr.String())
		}
		s.st.Since = time.Now().UnixMilli()
		s.mu.Unlock()
		r.OnChange(id)
	}()
	r.OnChange(id)
	return nil
}

// read follows the session's events: the agent's words and tools, its requests, the end of each turn.
func (r *Runner) read(id string, s *session, out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			SessionID string          `json:"session_id"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			ParentToolUseID *string `json:"parent_tool_use_id"`
			Result          string  `json:"result"`
			IsError         bool    `json:"is_error"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		changed := false
		s.mu.Lock()
		switch e.Type {
		case "system":
			if e.SessionID != "" {
				s.st.SessionID = e.SessionID
			}
		case "assistant":
			if e.ParentToolUseID != nil && *e.ParentToolUseID != "" {
				break // a subagent's
			}
			var blocks []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			json.Unmarshal(e.Message.Content, &blocks)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if t := strings.TrimSpace(b.Text); t != "" {
						s.st.LastText, s.st.Tool = t, ""
					}
				case "tool_use":
					s.st.Tool = orca.ToolLine(b.Name, b.Input)
				}
			}
			if !s.st.Busy {
				s.st.Busy, s.st.Since = true, time.Now().UnixMilli()
				changed = true
			}
		case "control_request":
			if req := parseRequest(e.RequestID, e.Request); req != nil {
				s.pending[req.ID] = req
				s.refreshLocked()
				changed = true
			}
		case "control_cancel_request":
			delete(s.pending, e.RequestID)
			s.refreshLocked()
			changed = true
		case "result":
			s.st.Busy, s.st.Tool, s.st.Since = false, "", time.Now().UnixMilli()
			if e.IsError || (e.Subtype != "" && e.Subtype != "success") {
				s.st.Error = strings.TrimSpace(firstNonEmpty(e.Result, "Хід агента завершився помилкою: "+e.Subtype))
			} else if t := strings.TrimSpace(e.Result); t != "" {
				s.st.LastText = t
			}
			// Nothing to do for a while: close the session; a message resumes it.
			stdin := s.stdin
			s.idle = time.AfterFunc(IdleAfter, func() { stdin.Close() })
			changed = true
		}
		s.mu.Unlock()
		if changed {
			r.OnChange(id)
		}
	}
}

func (s *session) refreshLocked() {
	s.st.Requests = s.st.Requests[:0]
	for _, req := range s.pending {
		s.st.Requests = append(s.st.Requests, *req)
	}
}

func parseRequest(id string, raw json.RawMessage) *Request {
	var req struct {
		Subtype     string          `json:"subtype"`
		ToolName    string          `json:"tool_name"`
		Input       json.RawMessage `json:"input"`
		Description string          `json:"description"`
		Suggestions json.RawMessage `json:"permission_suggestions"`
	}
	if json.Unmarshal(raw, &req) != nil || req.Subtype != "can_use_tool" {
		return nil
	}
	out := &Request{ID: id, Tool: req.ToolName, What: orca.ToolLine(req.ToolName, req.Input), Description: req.Description,
		At: time.Now().UnixMilli(), input: req.Input, suggestions: req.Suggestions}
	if req.ToolName == "AskUserQuestion" {
		var in struct {
			Questions []struct {
				Question    string `json:"question"`
				Header      string `json:"header"`
				MultiSelect bool   `json:"multiSelect"`
				Options     []struct {
					Label string `json:"label"`
				} `json:"options"`
			} `json:"questions"`
		}
		json.Unmarshal(req.Input, &in)
		for _, q := range in.Questions {
			qq := Question{Question: q.Question, Header: q.Header, Multi: q.MultiSelect}
			for _, o := range q.Options {
				qq.Options = append(qq.Options, o.Label)
			}
			out.Questions = append(out.Questions, qq)
		}
	}
	return out
}

// cleanEnv: Mako's environment without what a Claude Code session around it set (when Mako itself was started
// from one), so the agent is a session of its own. CLAUDE_CONFIG_DIR, the user's choice, stays.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDE") && !strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// tailBuffer keeps the last 2 KB of what a process says on stderr.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
