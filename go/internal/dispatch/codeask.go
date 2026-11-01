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
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// «Запитати про код»: a question about a project's code, without a task. A quick agent reads the project's clone
// with Read, Grep and Glob only (no worktree, no branch, nothing written or run) and answers in Mako's chat with
// file:line places the board opens. Each project keeps its questions; one can become a task's description.

// CodeTools: all the agent of a question may use.
var CodeTools = []string{"Read", "Grep", "Glob"}

// CodeModel: the quick model that answers.
const CodeModel = "haiku"

const codePrompt = "You answer the developer's questions about the code in this repository. You can only read it: " +
	"Read, Grep and Glob; never try to change files or run commands. Look the answer up in the code, then answer briefly, " +
	"in the language of the question. Cite every place you rely on as path:line (or path:start-end), the path relative to " +
	"the repository root, in backticks, e.g. `internal/web/web.go:120`."

// CodeProject: a clone the agent can read.
type CodeProject struct {
	Key     string `json:"key"` // tracker/project/repo
	Name    string `json:"name"`
	Tracker string `json:"tracker"`
	Project string `json:"project"`
	Path    string `json:"path"`
}

// CodeThread: one question and the conversation that grew from it.
type CodeThread struct {
	ID      string `json:"id"` // its chat: "ask-…"
	Project string `json:"project"`
	Title   string `json:"title"` // the first question
	Session string `json:"session,omitempty"`
	At      int64  `json:"at"` // the last question, unix ms
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
				out = append(out, CodeProject{Key: t.ID() + "/" + key + "/" + name, Name: label, Tracker: t.ID(), Project: key, Path: r.Path})
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
	return CodeProject{}, errors.New("немає клону цього проєкту на цьому Mac")
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
	return os.WriteFile(d.threadsFile(), b, 0o644)
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
		return "", errors.New("напишіть питання")
	}
	p, err := d.codeProject(project)
	if err != nil {
		return "", err
	}
	if thread != "" {
		t, ok := d.thread(thread)
		if !ok || t.Project != project {
			return "", errors.New("немає такого питання")
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
		return thread, d.startQuestion(p, t, text)
	}
	b := make([]byte, 6)
	rand.Read(b)
	t := CodeThread{ID: "ask-" + hex.EncodeToString(b), Project: project, Title: clipText(firstLine(text), 200), At: time.Now().UnixMilli()}
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
	return t.ID, d.startQuestion(p, t, text)
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

// CodeArgs: how claude runs for a question: the quick model, only the reading tools, no MCP servers.
func CodeArgs() []string {
	return []string{"--model", CodeModel, "--tools", strings.Join(CodeTools, ","), "--strict-mcp-config",
		"--disallowedTools", "Edit", "Write", "NotebookEdit", "Bash",
		"--append-system-prompt", codePrompt,
		"--allowedTools", strings.Join(CodeTools, ",")}
}

// startQuestion runs the agent in the clone with the question; a question asked before goes on with its
// conversation, shown from its transcript.
func (d *Dispatcher) startQuestion(p CodeProject, t CodeThread, text string) error {
	command, err := term.Resolve(d.Config().Terminal.Command)
	if err != nil {
		return err
	}
	args := CodeArgs()
	history := ""
	if t.Session != "" && orca.HasSession(p.Path, t.Session) {
		args = append([]string{"--resume", t.Session}, args...)
		history = orca.Transcript(p.Path, t.Session)
	}
	d.Chats.Put(chat.Start(chat.Options{
		ID: t.ID, Worktree: p.Path, Command: command, Args: args, Prompt: text, History: history, Tools: CodeTools,
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
	t, ok := d.thread(id)
	if !ok || t.Session == "" {
		return chat.Snapshot{}, false
	}
	p, err := d.codeProject(t.Project)
	// Its own session only: the clone's other conversations (yours, in your terminal) are not the question's.
	if err != nil || !orca.HasSession(p.Path, t.Session) {
		return chat.Snapshot{}, false
	}
	return chat.Replay(id, orca.Transcript(p.Path, t.Session)), true
}

// CodeFile: a file of the project's clone, for the board to show where the answer points, and its path in the
// clone. An answer may name a file short ("web.go:186"): then the one tracked file whose path ends so.
func (d *Dispatcher) CodeFile(ctx context.Context, project, path string) (text, found string, err error) {
	p, err := d.codeProject(project)
	if err != nil {
		return "", "", err
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
		return "", "", fmt.Errorf("немає файлу %s", path)
	}
	if st.Size() > 2<<20 {
		return "", "", fmt.Errorf("%s завеликий, щоб показати", path)
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
		return "", fmt.Errorf("немає файлу %s", path)
	}
	if rel, err := filepath.Rel(root, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s поза клоном проєкту", path)
	}
	return full, nil
}
