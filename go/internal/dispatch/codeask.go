package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// «Запитати про код»: a question about a project's code, without a task. The agent works in the project's one
// shared worktree on the base branch (mako-ask, next to the tasks' worktrees, brought up to date before each new
// conversation): it reads, searches and runs commands, but its tools that change files are refused. It answers in
// Mako's chat with file:line places the board opens, and may propose a ticket the developer edits and creates.
// Each project keeps its questions.

// CodeDeny: the tools the agent of a question may not use: it changes no code.
var CodeDeny = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// CodeModels: the models the picker offers. Claude Code has no command that lists them; its aliases always mean
// the newest of each.
var CodeModels = []string{"opus", "sonnet", "haiku"}

// TicketFence: the fenced block the agent proposes a ticket in, JSON {"title", "description"}.
const TicketFence = "mako-ticket"

const codePrompt = "You help the developer explore the code in this repository: answer questions, look for problems, " +
	"come up with ideas. You may read, search and run commands (tests, builds, git log, scripts), but never change, " +
	"create or delete files, and never change the repository (no git commit, push, checkout, reset, branch changes): " +
	"those tools and commands are refused, and do not work around it through the shell either. " +
	"Look the answer up in the code, then answer briefly, in the language of the question. Cite every place you rely " +
	"on as path:line (or path:start-end), the path relative to the repository root, in backticks, e.g. " +
	"`internal/web/web.go:120`. You cannot create tickets yourself. When the conversation finds a problem, a remark or " +
	"an idea worth a ticket, or the developer asks for one, end the answer with one fenced code block whose language " +
	"is " + TicketFence + " holding JSON {\"title\": \"…\", \"description\": \"…\"}: a short title and a Markdown " +
	"description (what, why, where in the code, how to check), in the language of the conversation. The developer " +
	"edits it and creates the ticket from it."

// askSlug: the folder of a project's shared worktree among the worktrees of its tasks.
const askSlug = "mako-ask"

// askMu: one change of a shared worktree at a time.
var askMu sync.Mutex

// CodeProject: a clone the agent can read.
type CodeProject struct {
	Key     string `json:"key"` // tracker/project/repo
	Name    string `json:"name"`
	Tracker string `json:"tracker"`
	Project string `json:"project"`
	Path    string `json:"path"`

	repo *config.Repo
}

// CodeThread: one question and the conversation that grew from it.
type CodeThread struct {
	ID      string `json:"id"` // its chat: "ask-…"
	Project string `json:"project"`
	Title   string `json:"title"` // the first question
	Session string `json:"session,omitempty"`
	// Dir: where its conversation runs, the shared worktree; "" — the clone, before the worktree.
	Dir string `json:"dir,omitempty"`
	At  int64  `json:"at"` // the last question, unix ms
}

// maxThreads: the questions a project keeps.
const maxThreads = 50

// CodeProjects: the projects of the trackers whose clone is on this Mac.
func (d *Dispatcher) CodeProjects() []CodeProject {
	out := []CodeProject{}
	seen := map[string]bool{}
	for _, t := range d.TrackerList() {
		d.mu.Lock()
		for key, p := range t.Config().Projects {
			for name, r := range p.Repos {
				if seen[r.Path] || !exists(filepath.Join(r.Path, ".git")) {
					continue
				}
				seen[r.Path] = true
				label := key
				if len(p.Repos) > 1 {
					label += " · " + name
				}
				out = append(out, CodeProject{Key: t.ID() + "/" + key + "/" + name, Name: label, Tracker: t.ID(), Project: key, Path: r.Path, repo: r})
			}
		}
		d.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (d *Dispatcher) codeProject(key string) (CodeProject, error) {
	for _, p := range d.CodeProjects() {
		if p.Key == key {
			return p, nil
		}
	}
	return CodeProject{}, errors.New("there is no clone of this project on this Mac")
}

func (d *Dispatcher) threadsFile() string {
	return filepath.Join(d.Config().StateDir, "questions.json")
}

func (d *Dispatcher) readThreads() []CodeThread {
	var all []CodeThread
	if b, err := os.ReadFile(d.threadsFile()); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

// updateThreads changes the kept questions under the lock.
func (d *Dispatcher) updateThreads(f func([]CodeThread) []CodeThread) error {
	d.questionsMu.Lock()
	defer d.questionsMu.Unlock()
	all := f(d.readThreads())
	b, _ := json.MarshalIndent(all, "", "  ")
	os.MkdirAll(filepath.Dir(d.threadsFile()), 0o755)
	return os.WriteFile(d.threadsFile(), b, 0o600)
}

// CodeThreads: a project's questions, the latest first.
func (d *Dispatcher) CodeThreads(project string) []CodeThread {
	d.questionsMu.Lock()
	all := d.readThreads()
	d.questionsMu.Unlock()
	out := []CodeThread{}
	for _, t := range all {
		if t.Project == project {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

func (d *Dispatcher) thread(id string) (CodeThread, bool) {
	d.questionsMu.Lock()
	defer d.questionsMu.Unlock()
	for _, t := range d.readThreads() {
		if t.ID == id {
			return t, true
		}
	}
	return CodeThread{}, false
}

// IsQuestion: the chat id is a question about code, not a task.
func IsQuestion(id string) bool { return strings.HasPrefix(id, "ask-") }

// AskCode asks about the project's code: a new question without thread, else one more in that conversation (a
// closed agent resumes it). It returns the question's id, its chat on the board.
func (d *Dispatcher) AskCode(project, thread, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("write a question")
	}
	p, err := d.codeProject(project)
	if err != nil {
		return "", err
	}
	if thread != "" {
		t, ok := d.thread(thread)
		if !ok || t.Project != project {
			return "", errors.New("no such question")
		}
		d.touchThread(thread, "")
		if s := d.Chats.Get(thread); s != nil && s.Running() {
			err := s.Send(text)
			if !errors.Is(err, chat.ErrClosed) {
				return thread, err
			}
			if err := d.awaitClosed(s); err != nil {
				return thread, err
			}
		}
		// The worktree is made again when it was removed, as it is otherwise: the conversation goes on in it.
		dir, setup, err := d.prepareAsk(p, false)
		if err != nil {
			return thread, err
		}
		if t.Dir != dir {
			// A question from before the shared worktree: its conversation was in the clone, a new one starts.
			t.Dir, t.Session = dir, ""
			d.setThreadDir(t.ID, dir)
		}
		return thread, d.launchQuestion(p, t, text, setup)
	}
	dir, setup, err := d.prepareAsk(p, true)
	if err != nil {
		return "", err
	}
	b := make([]byte, 6)
	rand.Read(b)
	t := CodeThread{ID: "ask-" + hex.EncodeToString(b), Project: project, Title: clipText(firstLine(text), 200), At: time.Now().UnixMilli(), Dir: dir}
	if err := d.updateThreads(func(all []CodeThread) []CodeThread {
		all = append(all, t)
		// A project keeps its latest questions.
		n := 0
		for i := len(all) - 1; i >= 0; i-- {
			if all[i].Project != project {
				continue
			}
			if n++; n > maxThreads {
				all = append(all[:i], all[i+1:]...)
			}
		}
		return all
	}); err != nil {
		return "", err
	}
	return t.ID, d.launchQuestion(p, t, text, setup)
}

// setThreadDir: the question's conversation runs in dir from now on, a new one.
func (d *Dispatcher) setThreadDir(id, dir string) {
	d.updateThreads(func(all []CodeThread) []CodeThread {
		for i := range all {
			if all[i].ID == id {
				all[i].Dir, all[i].Session = dir, ""
			}
		}
		return all
	})
}

// threadDir: where the question's conversation is, the clone for one from before the shared worktree.
func threadDir(p CodeProject, t CodeThread) string {
	if t.Dir != "" {
		return t.Dir
	}
	return p.Path
}

// touchThread: the question was asked again now; session, when given, is its Claude Code conversation.
func (d *Dispatcher) touchThread(id, session string) {
	d.updateThreads(func(all []CodeThread) []CodeThread {
		for i := range all {
			if all[i].ID == id {
				if session != "" {
					all[i].Session = session
				} else {
					all[i].At = time.Now().UnixMilli()
				}
			}
		}
		return all
	})
}

// CodeArgs: how claude runs for a question: the model picked in the settings, without MCP servers or the tools that
// edit; codeGate refuses the rest of what changes things.
func CodeArgs(model string) []string {
	if config.CheckModel(model) != nil {
		model = config.DefaultCodeModel
	}
	// No MCP servers: yours may create tickets or deploy, and the gate would let them through unasked. No settings
	// of yours or of the repository either: their allow rules (Bash(git push:*)) would run a command without asking
	// the gate.
	return append(append([]string{"--model", model, "--strict-mcp-config", "--setting-sources", "", "--disallowedTools"}, CodeDeny...),
		"--append-system-prompt", codePrompt)
}

// startQuestion runs the agent in the clone with the question; a question asked before goes on with its
// conversation, shown from its transcript.
func (d *Dispatcher) startQuestion(p CodeProject, t CodeThread, text string, setup *chat.SetupResult) error {
	command, err := term.Resolve(d.Config().Terminal.Command)
	if err != nil {
		return err
	}
	dir := threadDir(p, t)
	args := CodeArgs(d.Config().CodeModel)
	history := ""
	if t.Session != "" && orca.HasSession(dir, t.Session) {
		args = append([]string{"--resume", t.Session}, args...)
		history = orca.Transcript(dir, t.Session)
	}
	d.Chats.Put(chat.Start(chat.Options{
		ID: t.ID, Worktree: dir, Command: command, Args: args, Prompt: text, History: history, Gate: codeGate, SetupDone: setup,
		OnLimits:  d.limitsReported,
		OnSession: func(sid, _ string) { d.touchThread(t.ID, sid) },
	}))
	return nil
}

// QuestionChat: the conversation of a question whose agent is not running (closed while idle, or Mako restarted),
// from its transcript; ok is false when there is none.
func (d *Dispatcher) QuestionChat(id string) (chat.Snapshot, bool) {
	if !IsQuestion(id) {
		return chat.Snapshot{}, false
	}
	if s, ok := setupChat(id); ok {
		return s, true
	}
	t, ok := d.thread(id)
	if !ok || t.Session == "" {
		return chat.Snapshot{}, false
	}
	p, err := d.codeProject(t.Project)
	// Its own session only: the clone's other conversations (yours, in your terminal) are not the question's.
	if err != nil || !orca.HasSession(threadDir(p, t), t.Session) {
		return chat.Snapshot{}, false
	}
	return chat.Replay(id, orca.Transcript(threadDir(p, t), t.Session)), true
}

// CodeFile: a file of the project's shared worktree (the clone while there is none), for the board to show where
// the answer points, and its path there. An answer may name a file short ("web.go:186"): then the one tracked file
// whose path ends so.
func (d *Dispatcher) CodeFile(ctx context.Context, project, path string) (text, found string, err error) {
	p, err := d.codeProject(project)
	if err != nil {
		return "", "", err
	}
	if dir := askDir(p); exists(filepath.Join(dir, ".git")) {
		p.Path = dir
	}
	full, err := insideClone(p.Path, path)
	if err != nil && !filepath.IsAbs(path) {
		if rel := uniqueTracked(ctx, p.Path, path); rel != "" {
			full, err = insideClone(p.Path, rel)
		}
	}
	if err != nil {
		return "", "", err
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return "", "", fmt.Errorf("no such file %s", path)
	}
	if st.Size() > 2<<20 {
		return "", "", fmt.Errorf("%s is too large to show", path)
	}
	b, err := os.ReadFile(full)
	root, _ := filepath.EvalSymlinks(p.Path)
	found, _ = filepath.Rel(root, full)
	return string(b), filepath.ToSlash(found), err
}

// uniqueTracked: the clone's only tracked file whose path ends with path, "" when none or several.
func uniqueTracked(ctx context.Context, root, path string) string {
	path = strings.TrimPrefix(strings.TrimSpace(path), "./")
	if path == "" || strings.Contains(path, "..") {
		return ""
	}
	out, err := gitx.Run(ctx, root, "ls-files")
	if err != nil {
		return ""
	}
	match := ""
	for _, f := range strings.Split(out, "\n") {
		if f == path || strings.HasSuffix(f, "/"+path) {
			if match != "" {
				return ""
			}
			match = f
		}
	}
	return match
}

// insideClone: the path within the clone, symlinks resolved; anything outside it is refused.
func insideClone(root, path string) (string, error) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "./")
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, path)
	}
	full, err = filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("no such file %s", path)
	}
	if rel, err := filepath.Rel(root, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the project clone", path)
	}
	return full, nil
}
