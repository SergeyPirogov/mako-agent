package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// Mako's own terminal, the alternative to Orca: Mako makes the worktree and runs the agent in a terminal the
// board shows. The rest is the same: the agent pushes and opens the MR, Mako follows it; with checks in the
// repo's settings, Mako opens the MR itself once they pass (checks.go).

const terminalExecutor = "terminal"

func onTerminal(r core.Record) bool { return r.Executor == terminalExecutor }

// handTerminal makes the task's worktree and starts Claude in it with the same prompt Orca would get.
// Each step is one of the start's («запускаю»); a cancelled start returns what it made so far with ctx's error.
func (d *Dispatcher) handTerminal(ctx context.Context, j orca.Job, log func(string)) (*orca.Result, error) {
	cfg, id := d.Config(), j.Task.ID
	d.step(id, "main", "updating "+j.Repo.BaseBranch)
	fetchBase(ctx, j.Repo, log)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	d.step(id, "worktree", "creating the worktree")
	worktree, created, err := addWorktree(ctx, j.Repo, j.Task.Slug, j.Branch, log)
	if created {
		d.madeBranch(id)
	}
	made := &orca.Result{Worktree: worktree, Branch: j.Branch}
	if err != nil {
		if ctx.Err() != nil && worktree != "" && exists(worktree) {
			return made, ctx.Err() // half made: «Скасувати» takes it away
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	}
	j.Checks = gateChecks(cfg, j.Repo)
	prompt := orca.FullPrompt(j, cfg.Orca)
	d.notePrompt(j.Task.ID, prompt)
	var setup *chat.SetupResult
	if cmd := setupOf(j.Repo, cfg.Terminal, worktree); cmd != "" {
		d.startPlan(id, terminalExecutor, j.Repo.BaseBranch, true)
		d.step(id, "setup", "setup ("+clipText(cmd, 60)+")")
		setup = d.runSetup(ctx, id, worktree, cmd)
		if ctx.Err() != nil {
			return made, ctx.Err()
		}
		if setup.Failed {
			log("setup failed, the agent starts anyway:\n" + tailLines(setup.Output, 10))
		}
	}
	d.step(id, "agent", "the agent is starting")
	view := viewOf(cfg.Terminal)
	rec, _ := d.Store.Get(j.Task.ID)
	planning := wantsPlan(cfg.Terminal, j.Task, rec.Estimate, rec.PlanFirst)
	if m := modelFor(cfg, rec); m != "" && rec.Model == "" {
		log("model " + m + ": as the estimate advises")
	}
	d.Store.Update(j.Task.ID, func(r *core.Record) { r.View, r.Planning = view, planning })
	if planning {
		log("the agent starts with a plan: it changes nothing until you approve it")
	}
	if err := d.runAgent(j.Task.ID, worktree, view, prompt, false, setup); err != nil {
		return made, err
	}
	return &orca.Result{Worktree: worktree, Branch: j.Branch, Note: map[string]string{"chat": "Mako chat, ", "terminal": "Mako terminal, "}[view] + worktree}, nil
}

// viewOf: how a new task's agent is shown: the chat unless the settings say the terminal.
func viewOf(t config.Terminal) string {
	if t.View == "terminal" {
		return "terminal"
	}
	return "chat"
}

// runAgent starts Claude for a task in the view it is shown in: a conversation (the prompt as its first message)
// or a terminal; cont continues the worktree's last conversation instead.
// cont goes on with the task's conversation: its own Claude Code session when Mako knows it, else the newest
// in the worktree; a prompt with cont is your message that resumes it.
// setup: the worktree's setup Mako ran before it, shown in the chat (nil: none).
func (d *Dispatcher) runAgent(id, worktree, view, prompt string, cont bool, setup *chat.SetupResult) error {
	t := d.Config().Terminal
	r, _ := d.Store.Get(id)
	t.Model = terminalModel(d.Config(), r) // picked for the task, the estimate's advice, the configured one or Sonnet
	if !cont {
		d.Store.Update(id, func(r *core.Record) { r.AgentModel = t.Model }) // the chat names it exactly once it starts
	}
	again := []string{"--continue"}
	if orca.HasSession(worktree, r.SessionID) {
		again = []string{"--resume", r.SessionID}
	}
	if view != "chat" {
		// Claude Code's terminal asks to approve the plan itself; Mako only starts it in plan mode.
		if r.Planning {
			if !cont {
				t.PermissionMode = "plan"
			}
			d.Store.Update(id, func(r *core.Record) { r.Planning = false })
		}
		args := []string{prompt}
		if cont {
			// A message to go on with follows the session it continues; without one the agent waits at its prompt.
			args = again
			if prompt != "" {
				args = append(slices.Clone(again), prompt)
			}
		}
		return d.runClaude(id, worktree, t, args, setup, cont && prompt == "")
	}
	command, err := term.Resolve(t.Command)
	if err != nil {
		return err
	}
	var args []string
	if isClaude(command) && d.BoardURL != "" {
		settings, err := d.Sessions.GuardSettings(id, d.guardURL(id))
		if err != nil {
			return err
		}
		args = append(args, "--settings", settings)
	}
	if cont {
		args = append(args, again...)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if len(t.AllowedTools) > 0 {
		args = append(append(args, "--allowedTools"), t.AllowedTools...)
	}
	history := ""
	if cont {
		history = orca.Transcript(worktree, r.SessionID)
	}
	mode := t.PermissionMode
	if r.Planning {
		mode = "plan" // until the plan is approved, after Mako restarted too
	}
	if old := d.Chats.Get(id); old != nil {
		mode = old.Snapshot().Mode // a continued conversation keeps the mode you chose
	}
	d.Chats.Put(chat.Start(chat.Options{
		ID: id, Worktree: worktree, Command: command, Args: args, Env: d.agentEnv(id), SetupDone: setup, Prompt: prompt, Mode: mode, History: history,
		OnState:  func() { d.watchTerminals(time.Now()) },
		OnLimits: d.limitsReported,
		// The task keeps its session: a later resume goes on with exactly this conversation.
		OnSession: func(sid, model string) {
			d.Store.Update(id, func(r *core.Record) {
				r.SessionID = sid
				if model != "" {
					r.AgentModel = model
				}
			})
		},
		OnCost: func(usd float64, tokens int) {
			d.Store.Update(id, func(r *core.Record) {
				phase := core.PhaseWork
				if r.Planning {
					phase = core.PhasePlan
				}
				r.AddSpend(phase, usd, tokens)
			})
			d.recordContextLater(id) // the transcript may be long: not in the way of the conversation
		},
	}))
	return nil
}

// Live: the task's agent runs now, in its chat or its terminal.
func (d *Dispatcher) Live(id string) bool {
	if s := d.Chats.Get(id); s != nil && s.Running() {
		return true
	}
	s := d.Sessions.Get(id)
	return s != nil && s.Running()
}

// sayTo hands the task's running agent a request, in its chat or its terminal.
// Busy: Mako's own agents in the middle of a turn; they end with Mako, so its restart waits for them.
func (d *Dispatcher) Busy() int { return d.Sessions.Busy() + d.Chats.Busy() }

func (d *Dispatcher) sayTo(id, text string) error {
	if s := d.Chats.Get(id); s != nil && s.Running() {
		return s.Send(text)
	}
	if s := d.Sessions.Get(id); s != nil && s.Running() {
		s.Say(text)
		return nil
	}
	return fmt.Errorf("the agent is not running: press \"Continue\" first")
}

// SwitchView shows the task's agent the other way: it stops where it runs and continues the same conversation
// in the chat or the terminal.
func (d *Dispatcher) SwitchView(id, to string) error {
	if to != "chat" && to != "terminal" {
		return fmt.Errorf("unknown view %q", to)
	}
	r, ok := d.Store.Get(id)
	if !ok || !onTerminal(r) {
		return fmt.Errorf("the task is not in Mako")
	}
	if r.View == to || (r.View == "" && to == "terminal") {
		return nil
	}
	live := d.Live(id)
	d.StopTerminal(id)
	for i := 0; i < 50 && d.Live(id); i++ {
		time.Sleep(100 * time.Millisecond) // one claude at a time on a conversation
	}
	d.Store.Update(id, func(r *core.Record) { r.View = to })
	d.Log(id, map[string]string{"chat": "the agent is now in the chat", "terminal": "the agent is now in the terminal"}[to])
	if !live {
		return nil
	}
	return d.ResumeTerminal(id)
}

// ResumeTerminal starts Claude again in the task's worktree, continuing its last conversation: after Mako
// restarted or the agent was closed. Without a conversation to continue, it starts the task over there.
func (d *Dispatcher) ResumeTerminal(id string) error { return d.resume(id, "") }

// Say gives the task's agent your message: into its chat, or, when it is not running (closed while idle, or
// Mako restarted), by resuming its conversation with the message.
func (d *Dispatcher) Say(id, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := d.say(id, text); err != nil {
		return err
	}
	go d.suggestLesson(id, text)
	return nil
}

// awaitClosed waits for a session that refused a message (closed for being idle a moment ago, which stopped it
// already, or dead) to exit, so the conversation can be resumed with the message. It does not stop it: a dead
// agent still reports why it died. Up to 5s: Stop's SIGKILL comes after 3s. Say and AskCode wait that long.
func (d *Dispatcher) awaitClosed(s *chat.Session) error {
	select {
	case <-s.Done():
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("the agent is still closing: try again in a moment")
	}
}

func (d *Dispatcher) say(id, text string) error {
	if s := d.Chats.Get(id); s != nil && s.Running() {
		err := s.Send(text)
		if !errors.Is(err, chat.ErrClosed) {
			return err
		}
		if err := d.awaitClosed(s); err != nil {
			return err
		}
	}
	if d.Live(id) {
		return d.sayTo(id, text)
	}
	if err := d.resume(id, text); err != nil {
		return err
	}
	d.Log(id, "you wrote to the agent: "+clipText(firstLine(text), 120))
	return nil
}

// resume starts the agent again; message, when there is one, is what it goes on with.
func (d *Dispatcher) resume(id, message string) error {
	r, ok := d.Store.Get(id)
	if !ok || !onTerminal(r) || r.Worktree == "" {
		return fmt.Errorf("the task is not in a Mako terminal")
	}
	if d.Live(id) {
		return nil
	}
	if !exists(r.Worktree) {
		return fmt.Errorf("worktree %s was deleted", r.Worktree)
	}
	prompt, cont := message, true
	if !orca.HasTranscript(r.Worktree) {
		// No conversation was kept (the agent never got going, or ran without transcripts): start the task over
		// in the same worktree.
		t := d.tracker(r.Task.TrackerID)
		if t == nil {
			return fmt.Errorf("tracker %s was removed from the settings", r.Task.TrackerID)
		}
		repo, err := d.route(t, r.Task)
		if err != nil {
			return err
		}
		cfg := d.Config()
		j := orca.Job{Task: r.Task, TrackerName: t.Name(), Repo: repo, Branch: branchOf(r), RetryNote: r.RetryNote, LastAttempt: r.LastAttempt, Checks: gateChecks(cfg, repo), Language: cfg.AgentLanguage}
		prompt, cont = orca.FullPrompt(j, cfg.Orca), false
		d.notePrompt(id, prompt)
	}
	if err := d.runAgent(id, r.Worktree, r.View, prompt, cont, nil); err != nil {
		return err
	}
	if cont {
		d.Log(id, "the agent was started again")
	} else {
		d.Log(id, "the agent conversation was not saved: the task was started over in the same worktree")
	}
	return nil
}

// StopTerminal ends the task's agent; its worktree stays.
func (d *Dispatcher) StopTerminal(id string) {
	if s := d.Sessions.Get(id); s != nil {
		s.Stop()
	}
	if s := d.Chats.Get(id); s != nil {
		s.Stop()
	}
}

// setupOf: what prepares a new worktree: the repo's own setup, else the general one, else a guess from the
// lockfile; "-" is nothing.
func setupOf(repo *config.Repo, t config.Terminal, worktree string) string {
	s := strings.TrimSpace(repo.Setup)
	if s == "" {
		s = strings.TrimSpace(t.SetupCommand)
	}
	if s == "" {
		s = gitx.GuessSetup(worktree)
	}
	if s == "-" {
		return ""
	}
	return s
}

// runSetup prepares a new worktree before its agent starts (a failed setup does not stop it); «Скасувати» kills
// the setup with whatever it started.
// The output goes to a file, not a pipe: something the setup leaves running in the background would keep a pipe
// open, and the start would wait for it for ever.
func (d *Dispatcher) runSetup(ctx context.Context, id, worktree, setup string) *chat.SetupResult {
	res := &chat.SetupResult{Command: setup}
	f, err := os.CreateTemp("", "mako-setup-*.log")
	if err != nil {
		res.Output, res.Failed = err.Error(), true
		return res
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.Command("/bin/sh", "-c", setup)
	cmd.Dir, cmd.Env = worktree, append(term.CleanEnv(os.Environ()), d.agentEnv(id)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = f, f
	err = cmd.Start()
	if err == nil {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			case <-done:
			}
		}()
		err = cmd.Wait()
		close(done)
	}
	out, _ := os.ReadFile(f.Name())
	res.Output, res.Failed = string(out), err != nil
	return res
}

// clipTail: the last n bytes of s at most (an environment variable has a limit).
func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + strings.ToValidUTF8(s[len(s)-n:], "")
}

// tailLines: the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// The setup Mako ran is shown in the agent's terminal before the agent, so its output is there to read.
const setupShown = `printf '\033[2m› %s\033[0m\n' "$MAKO_SETUP"; printf '%s\n' "$MAKO_SETUP_OUT"; [ -n "$MAKO_SETUP_FAILED" ] && printf '\033[33mSetup failed, the agent starts anyway\033[0m\n'; exec "$0" "$@"`

func (d *Dispatcher) runClaude(id, worktree string, t config.Terminal, args []string, setup *chat.SetupResult, idle bool) error {
	command, err := term.Resolve(t.Command)
	if err != nil {
		return err
	}
	token := randomToken()
	var all []string
	if isClaude(command) {
		settings, err := d.Sessions.ClaudeSettings(id, fmt.Sprintf("%s/hook?id=%s&token=%s", d.BoardURL, escapeQuery(id), token), d.guardURL(id))
		if err != nil {
			return err
		}
		all = append([]string{"--settings", settings}, args...)
		// After the prompt: --allowedTools takes several values and would swallow it otherwise.
		if t.PermissionMode != "" && t.PermissionMode != "default" {
			all = append(all, "--permission-mode", t.PermissionMode)
		}
		if t.Model != "" {
			all = append(all, "--model", t.Model)
		}
		if len(t.AllowedTools) > 0 {
			all = append(append(all, "--allowedTools"), t.AllowedTools...)
		}
	} else {
		all = args
	}
	env := d.agentEnv(id)
	if setup != nil {
		all = append([]string{"-c", setupShown, command}, all...)
		command = "/bin/sh"
		env = append(env, "MAKO_SETUP="+setup.Command, "MAKO_SETUP_OUT="+clipTail(tailLines(setup.Output, 200), 16000))
		if setup.Failed {
			env = append(env, "MAKO_SETUP_FAILED=1")
		}
	}
	s, err := term.Start(term.Options{
		ID: id, Worktree: worktree, Command: command, Args: all, Token: token, Idle: idle,
		Env:     env,
		OnState: func() { d.watchTerminals(time.Now()) },
	})
	if err != nil {
		return fmt.Errorf("the agent failed to start: %w", err)
	}
	d.Sessions.Put(s)
	return nil
}

func isClaude(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "claude"
}

func randomToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func escapeQuery(s string) string {
	return strings.NewReplacer("%", "%25", "#", "%23", "&", "%26", "+", "%2B", " ", "%20", "/", "%2F").Replace(s)
}

// fetchBase brings the base branch up to date before a worktree starts from it; offline it starts from what is there.
func fetchBase(ctx context.Context, repo *config.Repo, log func(string)) {
	if _, err := gitx.Run(ctx, repo.Path, "fetch", "origin", repo.BaseBranch); err != nil && ctx.Err() == nil {
		log("failed to update " + repo.BaseBranch + ": " + err.Error())
	}
}

// addWorktree: <projects>/.worktrees/<repo>/<slug> on a new branch from the base branch, as Orca lays them out.
// A branch that exists already (a retry of the same branch) is checked out as it is; created: the branch is new.
// A failed add still returns the path, which may be left half made.
func addWorktree(ctx context.Context, repo *config.Repo, slug, branch string, log func(string)) (path string, created bool, err error) {
	dir := repo.WorktreesDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(repo.Path), ".worktrees", filepath.Base(repo.Path))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	path = filepath.Join(dir, slug)
	for i := 2; exists(path); i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s-%d", slug, i))
	}
	base := "origin/" + repo.BaseBranch
	if _, err := gitx.Run(ctx, repo.Path, "rev-parse", "--verify", "--quiet", base); err != nil {
		base = repo.BaseBranch
	}
	if _, err := gitx.Run(ctx, repo.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err = gitx.Run(ctx, repo.Path, "worktree", "add", path, branch)
		return path, false, err
	}
	if _, err := gitx.Run(ctx, repo.Path, "worktree", "add", "--no-track", "-b", branch, path, base); err != nil {
		return path, gitx.HasBranch(context.WithoutCancel(ctx), repo.Path, branch), err
	}
	log("worktree " + path + " on branch " + branch)
	return path, true, nil
}

// lookTerminal: the agent of a task in Mako's terminal, from its hooks.
func (d *Dispatcher) lookTerminal(r core.Record, now time.Time) Agent {
	a := Agent{Name: "claude", At: now.UnixMilli(), Terminal: true}
	if r.View == "chat" {
		return d.lookChat(r, a)
	}
	s := d.Sessions.Get(r.ID)
	if s == nil {
		a.State, a.Reason = "stopped", "no session: Mako was restarted"
	} else {
		st := s.State()
		a.Since = st.Since.UnixMilli()
		switch st.State {
		case "exited":
			a.State, a.Reason = "stopped", "the agent exited"
		case "waiting":
			a.State, a.Reason = "waiting", st.Reason
		case "done":
			a.State = "done"
		default:
			a.State, a.Tool = "working", st.Tool
		}
		a.Live = st.State != "exited"
	}
	if d.LastMessage != nil && r.Worktree != "" {
		a.Message = d.LastMessage(r.Worktree)
	}
	return a
}

// lookChat: the agent of a task in Mako's chat, from its conversation.
func (d *Dispatcher) lookChat(r core.Record, a Agent) Agent {
	s := d.Chats.Get(r.ID)
	if s == nil {
		a.State, a.Reason = "stopped", "no session: Mako was restarted"
		if d.LastMessage != nil && r.Worktree != "" {
			a.Message = d.LastMessage(r.Worktree)
		}
		return a
	}
	state, tool, since := s.State()
	a.Since, a.Live, a.Message = since.UnixMilli(), state != "exited", s.LastText()
	switch {
	case s.Snapshot().Asleep:
		// Closed while it waited for you: still its turn ended, a message resumes it.
		a.State = "done"
	case state == "exited":
		a.State, a.Reason = "stopped", "the agent exited"
	case state == "waiting" && strings.HasPrefix(tool, chat.PlanTool):
		a.State, a.Reason, a.Asked = "waiting", "the plan awaits approval", s.Asked()
	case state == "waiting" && strings.HasPrefix(tool, "AskUserQuestion"):
		a.State, a.Reason, a.Asked = "waiting", "the agent is asking", s.Asked()
	case state == "waiting":
		a.State, a.Reason, a.Asked = "waiting", "permission for "+tool, s.Asked()
	case state == "done":
		a.State = "done"
	default:
		a.State, a.Tool = "working", tool
	}
	if a.Message == "" && d.LastMessage != nil && r.Worktree != "" {
		a.Message = d.LastMessage(r.Worktree)
	}
	return a
}

// watchTerminals updates the agents in Mako's terminal right away: their hooks report every change.
func (d *Dispatcher) watchTerminals(now time.Time) {
	for _, r := range d.Store.All() {
		if r.Status == core.HandedOff && onTerminal(r) {
			d.setAgent(r, d.lookTerminal(r, now), now)
		}
		// A turn that ended with the work committed is for the checks before the MR.
		if d.checksFor(r) != nil && d.agentIdle(r.ID) {
			go d.maybeCheck(r.ID)
		}
		// A turn that ended after «Виправити» has the agent's report on the remarks.
		d.maybeReply(r)
	}
}

// Changes: what the task's branch changed against its base branch, committed or not.
func (d *Dispatcher) Changes(ctx context.Context, id string) (*gitx.ChangeSet, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return nil, err
	}
	cs, err := gitx.Changes(ctx, wt, base)
	if err == nil {
		cs.Branch, _ = gitx.Branch(ctx, wt, base)
	}
	return cs, err
}

// Editor: the code editor «Відкрити в редакторі» uses, "" when there is none.
func Editor() string {
	for _, e := range []struct{ name, app string }{{"Cursor", "/Applications/Cursor.app"}, {"Visual Studio Code", "/Applications/Visual Studio Code.app"}, {"Zed", "/Applications/Zed.app"}} {
		if exists(e.app) {
			return e.name
		}
	}
	return ""
}

// OpenWorktree shows the task's worktree in Finder ("finder") or the code editor ("editor").
func (d *Dispatcher) OpenWorktree(id, where, file string) error {
	wt, _, err := d.workOf(id)
	if err != nil {
		return err
	}
	if where == "editor" {
		if e := Editor(); e != "" {
			// One of the task's files, never a path out of its worktree.
			if f := filepath.Clean(file); file != "" && !filepath.IsAbs(f) && f != ".." && !strings.HasPrefix(f, "../") {
				return exec.Command("open", "-a", e, filepath.Join(wt, f)).Start()
			}
			return exec.Command("open", "-a", e, wt).Start()
		}
		return fmt.Errorf("no editor found (Cursor, VS Code, Zed)")
	}
	return exec.Command("open", wt).Start()
}

// FileDiff: one changed file of the task.
func (d *Dispatcher) FileDiff(ctx context.Context, id, path string, whole bool) (string, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return "", err
	}
	return gitx.FileDiff(ctx, wt, base, path, whole)
}

// workOf: the task's worktree and the branch it goes into.
func (d *Dispatcher) workOf(id string) (worktree, baseBranch string, err error) {
	r, ok := d.Store.Get(id)
	if !ok || r.Worktree == "" || !exists(r.Worktree) {
		return "", "", fmt.Errorf("the task has no worktree")
	}
	if t := d.tracker(r.Task.TrackerID); t != nil {
		if repo, err := d.route(t, r.Task); err == nil {
			if repo.BaseBranch != "" {
				return r.Worktree, repo.BaseBranch, nil
			}
			if exists(repo.Path) {
				return r.Worktree, gitx.DetectBaseBranch(repo.Path), nil
			}
		}
	}
	return r.Worktree, gitx.DetectBaseBranch(r.Worktree), nil
}

// History: the last n commits of the task's branch, for the commits' graph; as many as the panel fits, 10 when
// it does not say, never more than 200.
func (d *Dispatcher) History(ctx context.Context, id string, n int) (*gitx.History, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 10
	}
	return gitx.Log(ctx, wt, base, min(n, 200))
}

// CommitInfo: one commit of the task's worktree, its message and files.
func (d *Dispatcher) CommitInfo(ctx context.Context, id, hash string) (*gitx.CommitDetail, error) {
	wt, _, err := d.workOf(id)
	if err != nil {
		return nil, err
	}
	return gitx.CommitInfo(ctx, wt, hash)
}

// agentEnv: what Mako's agent knows of Mako: its task and the board, where `mako screenshot` hands screenshots.
func (d *Dispatcher) agentEnv(id string) []string {
	env := []string{"MAKO_TASK=" + id}
	if d.BoardURL != "" {
		env = append(env, "MAKO_URL="+d.BoardURL)
	}
	return env
}
