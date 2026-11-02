package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// A chat without a task: a conversation with the agent in a project, as with a task's agent but with no ticket.
// Each chat has its own worktree on its own branch (mako/chat-…) from the base branch; the agent runs as a task's
// does in the chat (the terminal settings' model, tools and permission mode): it may change the code, commit, push
// and open an MR. The chats are kept in chats.json; a message to a closed one resumes its Claude Code session.

// FreeChat: one chat without a task.
type FreeChat struct {
	ID      string `json:"id"` // "chat-…", its conversation on the board
	Project string `json:"project"`
	Title   string `json:"title"` // the first message
	Branch  string `json:"branch"`
	Dir     string `json:"dir"`
	Session string `json:"session,omitempty"`
	At      int64  `json:"at"` // the last message, unix ms
}

// IsFreeChat: the chat id is a chat without a task.
func IsFreeChat(id string) bool { return strings.HasPrefix(id, "chat-") }

func (d *Dispatcher) isFreeChat(id string) bool {
	_, ok := d.freeChat(id)
	return ok
}

func (d *Dispatcher) chatsFile() string { return filepath.Join(d.Config().StateDir, "chats.json") }

func (d *Dispatcher) readChats() []FreeChat {
	var all []FreeChat
	if b, err := os.ReadFile(d.chatsFile()); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

// updateChats changes the kept chats under the lock.
func (d *Dispatcher) updateChats(f func([]FreeChat) []FreeChat) error {
	d.chatsMu.Lock()
	defer d.chatsMu.Unlock()
	all := f(d.readChats())
	b, _ := json.MarshalIndent(all, "", "  ")
	os.MkdirAll(filepath.Dir(d.chatsFile()), 0o755)
	return os.WriteFile(d.chatsFile(), b, 0o600)
}

// FreeChats: the chats without a task, the latest first.
func (d *Dispatcher) FreeChats() []FreeChat {
	d.chatsMu.Lock()
	all := d.readChats()
	d.chatsMu.Unlock()
	if all == nil {
		all = []FreeChat{}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At > all[j].At })
	return all
}

func (d *Dispatcher) freeChat(id string) (FreeChat, bool) {
	if !IsFreeChat(id) {
		return FreeChat{}, false
	}
	d.chatsMu.Lock()
	defer d.chatsMu.Unlock()
	for _, c := range d.readChats() {
		if c.ID == id {
			return c, true
		}
	}
	return FreeChat{}, false
}

// touchChat: a message was written to the chat now; session, when given, is its Claude Code conversation.
func (d *Dispatcher) touchChat(id, session string) {
	d.updateChats(func(all []FreeChat) []FreeChat {
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

// chatRepo: the project's repo as addWorktree and setupOf take it, with its base branch known.
func chatRepo(p CodeProject) *config.Repo {
	repo := config.Repo{Path: p.Path}
	if p.repo != nil {
		repo = *p.repo
	}
	if repo.BaseBranch == "" {
		repo.BaseBranch = askBase(p)
	}
	return &repo
}

// NewChat opens a chat without a task in the project with its first message: its worktree on a new branch from
// the base branch, prepared as a task's is, then the agent. It returns the chat's id.
func (d *Dispatcher) NewChat(project, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("write a message")
	}
	p, err := d.codeProject(project)
	if err != nil {
		return "", err
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := "chat-" + hex.EncodeToString(b)
	repo, ctx := chatRepo(p), context.Background()
	fctx, cancel := context.WithTimeout(ctx, time.Minute)
	fetchBase(fctx, repo, func(string) {}) // offline the chat starts from what is there
	cancel()
	c := FreeChat{ID: id, Project: project, Title: clipText(firstLine(text), 200), Branch: "mako/" + id, At: time.Now().UnixMilli()}
	dir, _, err := addWorktree(ctx, repo, id, c.Branch, func(string) {})
	if err != nil {
		if dir != "" {
			os.RemoveAll(dir)
		}
		return "", errors.New("could not create the worktree for the chat: " + err.Error())
	}
	c.Dir = dir
	if err := d.updateChats(func(all []FreeChat) []FreeChat { return append(all, c) }); err != nil {
		return "", err
	}
	setup := setupOf(repo, d.Config().Terminal, dir)
	return id, d.launchAfterSetup(id, dir, text, setup, func(res *chat.SetupResult) error { return d.startFree(c, text, res) })
}

// startFree runs the chat's agent in its worktree, with the message (none: it waits); a chat that has a session
// goes on with it, shown from its transcript.
func (d *Dispatcher) startFree(c FreeChat, text string, setup *chat.SetupResult) error {
	t := d.Config().Terminal
	command, err := term.Resolve(t.Command)
	if err != nil {
		return err
	}
	model := t.Model
	if model == "" {
		model = FallbackModel
	}
	var args []string
	history := ""
	if c.Session != "" && orca.HasSession(c.Dir, c.Session) {
		args = append(args, "--resume", c.Session)
		history = orca.Transcript(c.Dir, c.Session)
	}
	args = append(args, "--model", model)
	if len(t.AllowedTools) > 0 {
		args = append(append(args, "--allowedTools"), slices.Clone(t.AllowedTools)...)
	}
	mode := t.PermissionMode
	if old := d.Chats.Get(c.ID); old != nil {
		mode = old.Snapshot().Mode // a continued conversation keeps the mode you chose
	}
	var env []string
	if d.BoardURL != "" {
		env = append(env, "MAKO_URL="+d.BoardURL)
	}
	d.Chats.Put(chat.Start(chat.Options{
		ID: c.ID, Worktree: c.Dir, Command: command, Args: args, Env: env, SetupDone: setup, Prompt: text, Mode: mode, History: history,
		OnLimits:  d.limitsReported,
		OnSession: func(sid, _ string) { d.touchChat(c.ID, sid) },
	}))
	return nil
}

// SayFree gives the chat's agent your message: into its conversation, or, when it is not running (closed while
// idle, or Mako restarted), by resuming the conversation with the message.
func (d *Dispatcher) SayFree(id, text string) error {
	if text = strings.TrimSpace(text); text == "" {
		return nil
	}
	return d.resumeFree(id, text)
}

// ResumeFree starts the chat's agent again on its conversation, waiting for your message.
func (d *Dispatcher) ResumeFree(id string) error { return d.resumeFree(id, "") }

func (d *Dispatcher) resumeFree(id, text string) error {
	c, ok := d.freeChat(id)
	if !ok {
		return errors.New("no such chat")
	}
	if text != "" {
		d.touchChat(id, "")
	}
	if s := d.Chats.Get(id); s != nil && s.Running() {
		if text == "" {
			return nil
		}
		err := s.Send(text)
		if !errors.Is(err, chat.ErrClosed) {
			return err
		}
		if err := d.awaitClosed(s); err != nil {
			return err
		}
	}
	if _, pending := askSetups.Load(id); pending {
		return errors.New("the worktree is still being prepared: try again in a moment")
	}
	if !exists(c.Dir) {
		return errors.New("the chat's worktree " + c.Dir + " was deleted")
	}
	return d.startFree(c, text, nil)
}

// FreeChatSnapshot: the conversation of a chat whose agent is not running, from its transcript (while its worktree
// is prepared, the setup); ok is false when there is none.
func (d *Dispatcher) FreeChatSnapshot(id string) (chat.Snapshot, bool) {
	if !IsFreeChat(id) {
		return chat.Snapshot{}, false
	}
	if s, ok := setupChat(id); ok {
		return s, true
	}
	c, ok := d.freeChat(id)
	if !ok || c.Session == "" || !orca.HasSession(c.Dir, c.Session) {
		return chat.Snapshot{}, false
	}
	return chat.Replay(id, orca.Transcript(c.Dir, c.Session)), true
}

// RemoveFreeChat: «×» — the chat's agent stops and its worktree goes, uncommitted changes and all; its branch
// stays, with what was committed on it.
func (d *Dispatcher) RemoveFreeChat(id string) error {
	c, ok := d.freeChat(id)
	if !ok {
		return errors.New("no such chat")
	}
	if s := d.Chats.Get(id); s != nil && s.Running() {
		stopChat(s)
	}
	if exists(c.Dir) {
		repo := c.Dir
		if p, err := d.codeProject(c.Project); err == nil {
			repo = p.Path
		}
		if err := gitx.DropWorktree(context.Background(), repo, c.Dir, ""); err != nil {
			if rmErr := os.RemoveAll(c.Dir); rmErr != nil {
				return err
			}
			if repo != c.Dir {
				gitx.Run(context.Background(), repo, "worktree", "prune")
			}
		}
	}
	return d.updateChats(func(all []FreeChat) []FreeChat {
		return slices.DeleteFunc(all, func(x FreeChat) bool { return x.ID == id })
	})
}
