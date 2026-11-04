package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// A chat without a task: a conversation with the agent in a project, as with a task's agent but with no ticket.
// Each chat has its own worktree on its own branch (mako/chat-…) from the base branch; the agent runs as a task's
// does in the chat (the terminal settings' model, tools and permission mode): it may change the code, commit, push
// and open an MR. The chats are kept in chats.json; a message to a closed one resumes its Claude Code session.

// FreeChat: one chat without a task. With no Project (and no Branch) it is a loose chat: the agent works in an
// empty folder of its own under <StateDir>/chats, with no repo, worktree or setup.
type FreeChat struct {
	ID      string `json:"id"` // "chat-…", its conversation on the board
	Project string `json:"project"`
	Title   string `json:"title"` // the first message
	Branch  string `json:"branch"`
	Dir     string `json:"dir"`
	Session string `json:"session,omitempty"`
	At      int64  `json:"at"` // the last message, unix ms
	// Error: why its worktree could not be made; the chat stays to read, it cannot go on.
	Error string `json:"error,omitempty"`
}

// Loose: the chat has no project: no clone, worktree or branch, only a folder of its own.
func (c FreeChat) Loose() bool { return c.Project == "" }

// looseRoot: the folder that holds the loose chats' folders.
func (d *Dispatcher) looseRoot() string { return filepath.Join(d.Config().StateDir, "chats") }

// freeMu: a chat's agent starts and a chat is removed one at a time, so no agent starts in a removed chat.
var freeMu sync.Mutex

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
	return fsx.WriteFile(d.chatsFile(), b, 0o600)
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

// chatTitle: the first line of the words of a first message; a message of pictures only is titled "Picture".
func chatTitle(text string) string {
	words, _ := chat.SplitImages(text)
	title := firstLine(words)
	if title == "" {
		title = "Picture"
	}
	return clipText(title, 200)
}

// NewChat opens a chat without a task with its first message and returns its id at once.
//
// With no project (""), the chat gets an empty folder of its own and the agent starts at once.
//
// In a project, the chat gets a worktree on a new branch from the base branch, prepared as a task's is. The agent
// comes in the background (prepareFree); the conversation shows the steps meanwhile.
func (d *Dispatcher) NewChat(project, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("write a message")
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := "chat-" + hex.EncodeToString(b)
	if project == "" {
		if err := d.newLooseChat(id, text); err != nil {
			return "", err
		}
		return id, nil
	}
	p, err := d.codeProject(project)
	if err != nil {
		return "", err
	}
	c := FreeChat{ID: id, Project: project, Title: chatTitle(text), Branch: "mako/" + id, At: time.Now().UnixMilli()}
	if err := d.updateChats(func(all []FreeChat) []FreeChat { return append(all, c) }); err != nil {
		return "", err
	}
	askSetups.Store(id, pendingSetup{command: "git worktree add -b " + c.Branch, prompt: text})
	go d.prepareFree(c, chatRepo(p), text)
	return id, nil
}

// newLooseChat opens a chat with no project: an empty folder of its own, the agent started at once with no
// worktree and no setup.
func (d *Dispatcher) newLooseChat(id, text string) error {
	dir := filepath.Join(d.looseRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	c := FreeChat{ID: id, Title: chatTitle(text), Dir: dir, At: time.Now().UnixMilli()}
	if err := d.updateChats(func(all []FreeChat) []FreeChat { return append(all, c) }); err != nil {
		os.RemoveAll(dir)
		return err
	}
	go func() {
		if err := d.startKept(id, text, nil); err != nil {
			log.Printf("[%s] agent did not start: %v", id, err)
		}
	}()
	return nil
}

// fetchTimeout: how long a new chat waits for the base branch to come; offline it starts from what is there.
var fetchTimeout = 30 * time.Second

// prepareFree makes the new chat's worktree, prepares it and starts the agent with the first message. A chat
// removed meanwhile is not started, and the worktree made for it goes.
func (d *Dispatcher) prepareFree(c FreeChat, repo *config.Repo, text string) {
	defer askSetups.Delete(c.ID)
	ctx := context.Background()
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	fetchBase(fctx, repo, func(string) {})
	cancel()
	dir, _, err := addWorktree(ctx, repo, c.ID, c.Branch, func(string) {})
	freeMu.Lock()
	_, kept := d.freeChat(c.ID)
	switch {
	case err != nil:
		if dir != "" {
			os.RemoveAll(dir)
		}
		if kept {
			msg := "could not create the worktree for the chat: " + err.Error()
			d.setChat(c.ID, func(x *FreeChat) { x.Error = msg })
		}
	case !kept:
		dropWorktree(repo.Path, dir)
	default:
		d.setChat(c.ID, func(x *FreeChat) { x.Dir = dir })
	}
	freeMu.Unlock()
	if err != nil || !kept {
		return
	}
	var res *chat.SetupResult
	if setup := setupOf(repo, d.Config().Terminal, dir); setup != "" {
		askSetups.Store(c.ID, pendingSetup{command: setup, prompt: text})
		res = d.runSetup(ctx, c.ID, dir, setup)
	}
	if err := d.startKept(c.ID, text, res); err != nil {
		log.Printf("[%s] agent did not start: %v", c.ID, err)
	}
}

// setChat changes the kept chat.
func (d *Dispatcher) setChat(id string, change func(*FreeChat)) {
	d.updateChats(func(all []FreeChat) []FreeChat {
		for i := range all {
			if all[i].ID == id {
				change(&all[i])
			}
		}
		return all
	})
}

// startKept starts the chat's agent unless the chat was removed.
func (d *Dispatcher) startKept(id, text string, setup *chat.SetupResult) error {
	freeMu.Lock()
	defer freeMu.Unlock()
	c, ok := d.freeChat(id)
	if !ok {
		return errors.New("no such chat")
	}
	return d.startFree(c, text, setup)
}

// dropWorktree takes a chat's worktree away, uncommitted changes and all.
func dropWorktree(repo, dir string) error {
	if err := gitx.DropWorktree(context.Background(), repo, dir, ""); err != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return err
		}
		gitx.Run(context.Background(), repo, "worktree", "prune")
	}
	return nil
}

// startFree runs the chat's agent in its worktree, with the message (none: it waits); a chat that has a session
// goes on with it, shown from its transcript.
func (d *Dispatcher) startFree(c FreeChat, text string, setup *chat.SetupResult) error {
	t := d.Config().Terminal
	command, err := term.Resolve(t.Command)
	if err != nil {
		return err
	}
	// The route of a task's work with no estimate (medium), in the project's routes mode.
	route := d.chatRoute(c)
	var args []string
	history := ""
	if c.Session != "" && orca.HasSession(c.Dir, c.Session) {
		args = append(args, "--resume", c.Session)
		history = orca.Transcript(c.Dir, c.Session)
	}
	if route.Model != "" {
		args = append(args, "--model", route.Model)
	}
	args = append(args, effortArgs(route.Effort)...)
	if len(t.AllowedTools) > 0 {
		args = append(append(args, "--allowedTools"), slices.Clone(t.AllowedTools)...)
	}
	mode := t.PermissionMode
	if old := d.Chats.Get(c.ID); old != nil {
		mode = old.Snapshot().Mode // a continued conversation keeps the mode you chose
	}
	d.Chats.Put(chat.Start(chat.Options{
		ID: c.ID, Worktree: c.Dir, Command: command, Args: args, Env: d.agentEnv(c.ID), SetupDone: setup, Prompt: text, Mode: mode, History: history,
		OnLimits:  d.limitsReported,
		OnResume:  d.afterLimit(c.Title, func(text string) error { return d.SayFree(c.ID, text) }),
		OnSession: func(sid, _ string) { d.touchChat(c.ID, sid) },
	}))
	return nil
}

// chatRoute: the model and effort of the chat's agent: the routes' work of a task with no estimate, in the mode of
// the chat's repo.
func (d *Dispatcher) chatRoute(c FreeChat) core.Route {
	mode := ""
	if p, err := d.codeProject(c.Project); err == nil && p.repo != nil {
		mode = p.repo.RouteMode
	}
	return routeFor(d.Config(), core.Record{}, core.PhaseWork, mode)
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
	if c.Error != "" {
		return errors.New(c.Error)
	}
	if c.Dir == "" || !exists(c.Dir) {
		if c.Loose() {
			return errors.New("the chat's folder " + c.Dir + " was deleted")
		}
		return errors.New("the chat's worktree " + c.Dir + " was deleted")
	}
	return d.startKept(id, text, nil)
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
	if ok && c.Error != "" {
		return chat.Drawn(chat.Snapshot{State: "exited", Items: []chat.Item{{Kind: "user", Text: c.Title}, {Kind: "error", Text: c.Error}}}, ""), true
	}
	if !ok || c.Session == "" || !orca.HasSession(c.Dir, c.Session) {
		return chat.Snapshot{}, false
	}
	return chat.Replay(id, orca.Transcript(c.Dir, c.Session)), true
}

// RemoveFreeChat: «×» — the chat's agent stops and its worktree goes, uncommitted changes and all; its branch
// stays, with what was committed on it.
// A chat with no project loses its whole folder.
// A chat whose worktree is still being made is removed too: what was made for it goes once it is (prepareFree).
func (d *Dispatcher) RemoveFreeChat(id string) error {
	freeMu.Lock()
	defer freeMu.Unlock()
	c, ok := d.freeChat(id)
	if !ok {
		return errors.New("no such chat")
	}
	if err := d.updateChats(func(all []FreeChat) []FreeChat {
		return slices.DeleteFunc(all, func(x FreeChat) bool { return x.ID == id })
	}); err != nil {
		return err
	}
	if s := d.Chats.Get(id); s != nil {
		stopChat(s)
		d.Chats.Delete(id)
	}
	if c.Dir == "" || !exists(c.Dir) {
		return nil
	}
	if c.Loose() {
		// Only a folder under <StateDir>/chats goes: chats.json is not a license to delete anything else.
		root := d.looseRoot()
		if rel, err := filepath.Rel(root, c.Dir); err != nil || rel == "." || !filepath.IsLocal(rel) {
			return fmt.Errorf("the chat's folder %s is not inside %s: left in place", c.Dir, root)
		}
		return os.RemoveAll(c.Dir)
	}
	repo := c.Dir
	if p, err := d.codeProject(c.Project); err == nil {
		repo = p.Path
	}
	return dropWorktree(repo, c.Dir)
}

// FreePlan answers the plan the chat's agent offers as a task's is answered (do: approve with the mode, model and
// notes picked; revise with your notes; reject), but nothing is noted in the task store: that would make a task of
// the chat.
func (d *Dispatcher) FreePlan(id, requestID, do string, o PlanApproval) error {
	c := d.Chats.Get(id)
	if c == nil {
		return errors.New("the agent is not running")
	}
	switch do {
	case "approve":
		mode, err := d.approvalMode(o.Mode)
		if err != nil {
			return err
		}
		err = c.ApprovePlan(requestID, mode, strings.TrimSpace(o.Model), o.Notes.Text())
		if errors.Is(err, chat.ErrApprovedButNot) {
			log.Printf("[%s] %v", id, err) // approved: the agent goes on, what failed after it is only said
			return nil
		}
		return err
	case "revise":
		return c.RevisePlan(requestID, o.Notes.Text())
	case "reject":
		return c.RejectPlan(requestID)
	}
	return fmt.Errorf("unknown plan action %q", do)
}
