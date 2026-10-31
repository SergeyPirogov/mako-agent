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
	onState func()
}

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
}

// Start runs the command in a new pseudo-terminal.
func Start(o Options) (*Session, error) {
	cmd := exec.Command(o.Command, o.Args...)
	cmd.Dir = o.Worktree
	cmd.Env = append(cleanEnv(os.Environ()), append([]string{"TERM=xterm-256color", "COLORTERM=truecolor", "FORCE_COLOR=1"}, o.Env...)...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 34})
	if err != nil {
		return nil, err
	}
	s := &Session{ID: o.ID, Worktree: o.Worktree, Token: o.Token, cmd: cmd, pty: f, subs: map[chan Event]struct{}{},
		state: "working", since: time.Now(), onState: o.OnState}
	go s.read()
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
	s.cmd.Wait()
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
}

// The marks of a Claude Code session Mako may have been started from: an agent that inherits them takes itself
// for that session's child and saves no transcript (so no last message and no --continue).
var sessionMarks = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_EFFORT": true, "CLAUDE_CODE_ENTRYPOINT": true,
	"CLAUDE_CODE_EXECPATH": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
	"CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
}

func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !sessionMarks[k] {
			out = append(out, kv)
		}
	}
	return out
}

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

// Stop ends the agent: hang up its terminal, then kill it if it is still there.
func (s *Session) Stop() {
	if !s.Running() {
		return
	}
	if p := s.cmd.Process; p != nil {
		syscall.Kill(-p.Pid, syscall.SIGHUP)
		p.Signal(syscall.SIGHUP)
		go func() {
			time.Sleep(3 * time.Second)
			if s.Running() {
				p.Kill()
			}
		}()
	}
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
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return err
	}
	switch e.Event {
	case "UserPromptSubmit", "PostToolUse":
		s.set("working", "", "")
	case "PreToolUse":
		s.set("working", "", toolLine(e.ToolName, e.ToolInput))
	case "Notification":
		s.set("waiting", Humanize(e.Message), "")
	case "Stop":
		s.set("done", "", "")
	default:
		return fmt.Errorf("unknown hook %q", e.Event)
	}
	return nil
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

// Humanize: Claude Code's notifications in Ukrainian; others as they are.
func Humanize(m string) string {
	switch {
	case permission.MatchString(m):
		return "дозвіл на " + permission.FindStringSubmatch(m)[1]
	case regexp.MustCompile(`(?i)waiting for your input`).MatchString(m):
		return "чекає на вашу відповідь"
	case regexp.MustCompile(`(?i)needs your (approval|attention)`).MatchString(m):
		return "потрібне ваше рішення"
	case m == "":
		return "питання або дозвіл"
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

// ClaudeSettings writes the settings that add Mako's hooks (Claude Code merges them with the user's own) and
// returns the file.
func (m *Manager) ClaudeSettings(name, hookURL string) (string, error) {
	post := map[string]any{"type": "command", "command": `curl -s -m 3 -X POST -H "Content-Type: application/json" --data-binary @- "` + hookURL + `"`}
	hooks := map[string]any{"hooks": []any{post}}
	withAll := map[string]any{"matcher": "*", "hooks": []any{post}}
	settings := map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": []any{hooks},
		"PreToolUse":       []any{withAll},
		// After a permission is given the tool runs: back from «waiting» to «working».
		"PostToolUse":  []any{withAll},
		"Notification": []any{hooks},
		"Stop":         []any{hooks},
	}}
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return "", err
	}
	file := filepath.Join(m.Dir, safeName.ReplaceAllString(name, "_")+".claude-settings.json")
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
			return "", fmt.Errorf("немає %s", command)
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
	return "", fmt.Errorf("не знайдено команду «%s»: вкажіть повний шлях у Налаштуваннях → Агент (%w)", command, ErrNoCommand)
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
