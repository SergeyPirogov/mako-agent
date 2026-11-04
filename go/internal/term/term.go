// Package term runs the agent in a pseudo-terminal that the board shows live and you can type into: Mako's
// own terminal, the alternative to Orca. Claude Code's hooks tell Mako whether the agent works, waits for you
// or has finished its turn.
package term

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/proc"
	"github.com/creack/pty"
)

// What a terminal keeps for a viewer who comes later: the last 2 MB of output.
const maxBuffer = 2 << 20

// Event goes to the viewers: "o" output, "x" the agent's process ended.
type Event struct {
	T string `json:"t"`
	D string `json:"d,omitempty"`
}

// Session is one agent in a terminal.
type Session struct {
	ID       string // the task's id
	Worktree string
	Token    string // the hooks' password: only this agent may report its state

	mu      sync.Mutex
	cmd     *exec.Cmd
	pty     *os.File
	buf     []byte
	subs    map[chan Event]struct{}
	state   string // "working", "waiting", "done"
	reason  string
	tool    string
	since   time.Time
	exited  bool
	session string // Claude Code's session id, from its hooks
	onState func()
	onSess  func(string)
	done    chan struct{} // closed once the process ended and onState ran
	drained chan struct{} // closed once the terminal gave its last output
}

// stopGrace: how long Stop waits after hanging up before it kills the agent and all it started.
var stopGrace = 3 * time.Second

// drainGrace: how long the end of the process waits for the terminal's last output. A descendant that outlived it
// (an MCP server, a shell in a group of its own) may hold the terminal open: the agent has ended all the same.
var drainGrace = time.Second

// State: what the agent does now, from its hooks. "exited" once its process ended.
type State struct {
	State  string
	Reason string
	Tool   string
	Since  time.Time
}

// Options start one session.
type Options struct {
	ID, Worktree string
	Command      string
	Args         []string
	Env          []string
	Token        string
	OnState      func() // the agent's state changed
	// OnSession: Claude Code named its session (the first hook, or a new conversation after /clear).
	OnSession func(sessionID string)
	// Idle: the agent starts without a request (it continues a conversation) and waits at its prompt.
	Idle bool
}

// Start runs the command in a new pseudo-terminal.
func Start(o Options) (*Session, error) {
	cmd := exec.Command(o.Command, o.Args...)
	cmd.Dir = o.Worktree
	cmd.Env = append(proc.CleanEnv(os.Environ()), append([]string{"TERM=xterm-256color", "COLORTERM=truecolor", "FORCE_COLOR=1"}, o.Env...)...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 34})
	if err != nil {
		return nil, err
	}
	state := "working"
	if o.Idle {
		state = "done"
	}
	s := &Session{ID: o.ID, Worktree: o.Worktree, Token: o.Token, cmd: cmd, pty: f, subs: map[chan Event]struct{}{},
		state: state, since: time.Now(), onState: o.OnState, onSess: o.OnSession, done: make(chan struct{}), drained: make(chan struct{})}
	go s.read()
	go s.wait()
	return s, nil
}

func (s *Session) read() {
	b := make([]byte, 32<<10)
	var rest []byte // a UTF-8 character cut between two reads
	for {
		n, err := s.pty.Read(b)
		if n > 0 {
			chunk := append(rest, b[:n]...)
			cut := validPrefix(chunk)
			rest = append([]byte{}, chunk[cut:]...)
			s.emit(chunk[:cut])
		}
		if err != nil {
			break
		}
	}
	close(s.drained)
}

// wait: the agent ends with its process, not with its terminal, which a descendant may still hold.
func (s *Session) wait() {
	s.cmd.Wait()
	select {
	case <-s.drained:
	case <-time.After(drainGrace):
	}
	s.mu.Lock()
	s.exited = true
	s.since = time.Now()
	for c := range s.subs {
		select {
		case c <- Event{T: "x"}:
		default:
		}
		close(c)
	}
	s.subs = map[chan Event]struct{}{}
	s.mu.Unlock()
	s.pty.Close()
	s.changed()
	close(s.done)
}

// Wait blocks until the process has ended and its last state change was reported.
func (s *Session) Wait() { <-s.done }

// validPrefix: the bytes up to a UTF-8 character the read cut in two.
func validPrefix(b []byte) int {
	for i := len(b); i > 0 && i > len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i-1]) {
			if utf8.FullRune(b[i-1:]) {
				return len(b)
			}
			return i - 1
		}
	}
	return len(b)
}

func (s *Session) emit(d []byte) {
	if len(d) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, d...)
	if len(s.buf) > maxBuffer {
		s.buf = s.buf[len(s.buf)-maxBuffer:]
		s.buf = s.buf[validStart(s.buf):]
	}
	for c := range s.subs {
		select {
		case c <- Event{T: "o", D: string(d)}:
		default:
			// A viewer that does not keep up starts over: it reconnects and gets the whole buffer.
			delete(s.subs, c)
			close(c)
		}
	}
}

func validStart(b []byte) int {
	for i := 0; i < len(b) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return 0
}

// Subscribe: the output so far and a channel of what comes next; nil when the agent has ended.
func (s *Session) Subscribe() (string, chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return string(s.buf), nil
	}
	c := make(chan Event, 256)
	s.subs[c] = struct{}{}
	return string(s.buf), c
}

func (s *Session) Unsubscribe(c chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[c]; ok {
		delete(s.subs, c)
		close(c)
	}
}

// Subscribers: how many boards watch the session now.
func (s *Session) Subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// Input is what you type.
func (s *Session) Input(d string) {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if !exited {
		s.pty.WriteString(d)
	}
}

func (s *Session) Resize(cols, rows int) {
	if cols > 10 && rows > 3 && cols < 1000 && rows < 500 {
		pty.Setsize(s.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

// Say gives the agent a request as if you pasted it into its prompt and pressed Enter.
func (s *Session) Say(text string) bool {
	if !s.Running() {
		return false
	}
	s.pty.WriteString("\x1b[200~" + text + "\x1b[201~")
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.pty.WriteString("\r")
	}()
	return true
}

// Running: the agent's process has not ended.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exited
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return State{State: "exited", Since: s.since}
	}
	return State{State: s.state, Reason: s.reason, Tool: s.tool, Since: s.since}
}

// Output: the end of what the terminal showed, as plain text.
func (s *Session) Output(max int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PlainTail(string(s.buf), max)
}

// Stop ends the agent: hang up its terminal, then kill it and all it started if it is still there.
func (s *Session) Stop() {
	if !s.Running() {
		return
	}
	p := s.cmd.Process
	if p == nil {
		return
	}
	// The terminal starts the agent in a session of its own: its process group is the agent's pid.
	syscall.Kill(-p.Pid, syscall.SIGHUP)
	grace := stopGrace // read here: the goroutine below may outlive a test that restores stopGrace
	go func() {
		select {
		case <-s.done:
			return
		case <-time.After(grace):
		}
		proc.KillGroup(p.Pid)
		s.pty.Close()
	}()
}

func (s *Session) set(state, reason, tool string) {
	s.mu.Lock()
	if s.exited || (state == s.state && reason == s.reason && tool == s.tool) {
		s.mu.Unlock()
		return
	}
	if state != s.state {
		s.since = time.Now()
	}
	s.state, s.reason, s.tool = state, reason, tool
	s.mu.Unlock()
	s.changed()
}

func (s *Session) changed() {
	if s.onState != nil {
		s.onState()
	}
}

// Hook is one event from Claude Code's hooks.
func (s *Session) Hook(body []byte) error {
	var e struct {
		Event     string          `json:"hook_event_name"`
		Message   string          `json:"message"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		SessionID string          `json:"session_id"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return err
	}
	s.noteSession(e.SessionID)
	switch e.Event {
	case "UserPromptSubmit", "PostToolUse":
		s.set("working", "", "")
	case "PreToolUse":
		s.set("working", "", toolLine(e.ToolName, e.ToolInput))
	case "Notification":
		// What it waits on: the tool it asked for last («Bash: npm run build»), for the panel that answers it.
		s.mu.Lock()
		tool := s.tool
		s.mu.Unlock()
		s.set("waiting", Humanize(e.Message), tool)
	case "Stop":
		s.set("done", "", "")
	default:
		return fmt.Errorf("unknown hook %q", e.Event)
	}
	return nil
}

// noteSession keeps the session id the hooks name and tells OnSession when it is a new one.
func (s *Session) noteSession(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	changed := id != s.session
	s.session = id
	s.mu.Unlock()
	if changed && s.onSess != nil {
		s.onSess(id)
	}
}

// SessionID: Claude Code's session the agent runs, "" until its first hook.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// toolLine: "Bash: npm test", "Edit: src/app.ts".
func toolLine(name string, input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "pattern", "url", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			v = strings.TrimSpace(strings.SplitN(v, "\n", 2)[0])
			if r := []rune(v); len(r) > 160 {
				v = string(r[:159]) + "…"
			}
			return name + ": " + v
		}
	}
	return name
}

var permission = regexp.MustCompile(`(?i)needs your permission to use (.+)$`)

// Humanize: Claude Code's notifications in short form; others as they are.
func Humanize(m string) string {
	switch {
	case permission.MatchString(m):
		return "permission for " + permission.FindStringSubmatch(m)[1]
	case regexp.MustCompile(`(?i)waiting for your input`).MatchString(m):
		return "waiting for your reply"
	case regexp.MustCompile(`(?i)needs your (approval|attention)`).MatchString(m):
		return "needs your decision"
	case m == "":
		return "question or permission"
	}
	return m
}

var (
	oscSeq  = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	csiSeq  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	escSeq  = regexp.MustCompile(`\x1b[@-_]`)
	blankLn = regexp.MustCompile(`\n{3,}`)
)

// PlainTail: the last max characters of a terminal recording without its escape codes.
func PlainTail(out string, max int) string {
	out = oscSeq.ReplaceAllString(out, "")
	out = csiSeq.ReplaceAllString(out, "")
	out = escSeq.ReplaceAllString(out, "")
	out = blankLn.ReplaceAllString(strings.ReplaceAll(out, "\r", ""), "\n\n")
	out = strings.TrimSpace(out)
	if r := []rune(out); len(r) > max {
		out = string(r[len(r)-max:])
	}
	return out
}

// Manager keeps the sessions of all tasks.
type Manager struct {
	Dir string // where the agents' settings files go
	mu  sync.Mutex
	all map[string]*Session
}

func NewManager(dir string) *Manager {
	return &Manager{Dir: dir, all: map[string]*Session{}}
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all[id]
}

// Put replaces the task's session, stopping the one before.
func (m *Manager) Put(s *Session) {
	m.mu.Lock()
	old := m.all[s.ID]
	m.all[s.ID] = s
	m.mu.Unlock()
	if old != nil {
		old.Stop()
	}
}

// StopAll ends every agent: Mako quits.
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
		if st := s.State(); st.State == "working" {
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

// ClaudeSettings writes the settings that add Mako's hooks (Claude Code merges them with the user's own) and
// returns the file. guardURL, when set, checks the agent's pushes for secrets (see GuardSettings).
func (m *Manager) ClaudeSettings(name, hookURL, guardURL string) (string, error) {
	post := map[string]any{"type": "command", "command": `curl -s -m 3 -X POST -H "Content-Type: application/json" --data-binary @- "` + hookURL + `"`}
	hooks := map[string]any{"hooks": []any{post}}
	withAll := map[string]any{"matcher": "*", "hooks": []any{post}}
	pre := []any{withAll}
	if guardURL != "" {
		pre = append(pre, guardHook(guardURL))
	}
	settings := map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": []any{hooks},
		"PreToolUse":       pre,
		// After a permission is given the tool runs: back from «waiting» to «working».
		"PostToolUse":  []any{withAll},
		"Notification": []any{hooks},
		"Stop":         []any{hooks},
	}}
	return m.writeSettings(name+".claude-settings.json", settings)
}

// GuardSettings writes the settings of an agent in Mako's chat: only the push guard, its state comes another
// way. Before each Bash command Claude Code asks guardURL; Mako answers «deny» with the reason for a push that
// would carry secrets and nothing otherwise. Mako not answering does not stop the command.
func (m *Manager) GuardSettings(name, guardURL string) (string, error) {
	return m.writeSettings(name+".guard-settings.json", map[string]any{"hooks": map[string]any{"PreToolUse": []any{guardHook(guardURL)}}})
}

func guardHook(guardURL string) map[string]any {
	ask := map[string]any{"type": "command", "timeout": 60,
		"command": `curl -s -m 45 -X POST -H "Content-Type: application/json" --data-binary @- "` + guardURL + `" || true`}
	return map[string]any{"matcher": "Bash", "hooks": []any{ask}}
}

func (m *Manager) writeSettings(name string, settings map[string]any) (string, error) {
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return "", err
	}
	file := filepath.Join(m.Dir, safeName.ReplaceAllString(name, "_"))
	return file, os.WriteFile(file, b, 0o600)
}

var safeName = regexp.MustCompile(`[^\w.-]`)

// ErrNoCommand: the agent's command is not found.
var ErrNoCommand = errors.New("command not found")

// Resolve finds the agent's command: as set, else on PATH, else where installers put claude.
func Resolve(command string) (string, error) {
	if command == "" {
		command = "claude"
	}
	if strings.Contains(command, "/") {
		command = expandHome(command)
		if _, err := os.Stat(command); err != nil {
			return "", fmt.Errorf("no %s", command)
		}
		return command, nil
	}
	if p, err := exec.LookPath(command); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, ".claude", "local"), "/opt/homebrew/bin", "/usr/local/bin"} {
		if p := filepath.Join(dir, command); isFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("command \"%s\" not found: set the full path in Settings → Agent (%w)", command, ErrNoCommand)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
