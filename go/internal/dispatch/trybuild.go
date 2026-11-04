package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/proc"
	"github.com/SergeyPirogov/mako-agent/go/internal/shot"
)

// «Try this build»: the task's branch runs as a second copy of the app, for you to click through the steps of
// «Check it yourself». In Mako's own repository it is Mako built from the worktree, on a free port with a copy of
// the data (shot.ServeWorktree in shot.Try mode: nothing reaches the trackers, agents or repositories); in another
// project, its repo's "try" command in the worktree with a free port in $PORT. One copy per task, kept while its
// card is open (the board's look keeps it, TryKeep) or until «Stop».

// TryView is the task's copy to try as the board shows it.
type TryView struct {
	// State: "starting" (building, coming up), "running", "failed".
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
	// Fresh: the copy runs the branch as it is now.
	Fresh bool `json:"fresh,omitempty"`
}

// tryRun is a copy to try of a task's branch at head.
type tryRun struct {
	state, url, err, head string
	stop                  func()
	cancel                context.CancelFunc
	seen                  time.Time
}

// tryIdle: a copy no board has looked at for this long is stopped (its card was closed).
const tryIdle = 90 * time.Second

// tryWait: how long a project's try command may take to listen on its port.
const tryWait = 2 * time.Minute

// serveMako builds the Mako of the worktree and runs it as a copy to try with a copy of the data in home
// (Dispatcher.ServeMako in its place, in tests).
func serveMako(ctx context.Context, worktree, home string) (url string, stop func(), err error) {
	url, cleanup, err := shot.ServeWorktree(ctx, worktree, "", home, shot.Try)
	if err != nil && cleanup != nil {
		cleanup()
		cleanup = nil
	}
	return url, cleanup, err
}

// RunTry starts a project's try command in its worktree with port in $PORT and returns once something listens
// there; stop ends the command with all it started. A variable for tests.
var RunTry = func(ctx context.Context, dir, command string, port int) (stop func(), err error) {
	out, err := os.CreateTemp("", "mako-try-*.log")
	if err != nil {
		return nil, err
	}
	logName := out.Name()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(proc.CleanEnv(os.Environ()), "PORT="+strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		out.Close()
		os.Remove(logName)
		return nil, err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); out.Close(); close(exited) }()
	pid := cmd.Process.Pid
	stop = func() {
		_ = syscall.Kill(-pid, syscall.SIGINT)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = proc.KillGroup(pid)
			<-exited
		}
		_ = proc.KillGroup(pid) // what the command left running in its group
		os.Remove(logName)
	}
	logTail := func() string {
		b, _ := os.ReadFile(logName)
		s := strings.TrimSpace(string(b))
		if len(s) > 1500 {
			s = "…" + s[len(s)-1500:]
		}
		return s
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for end := time.Now().Add(tryWait); time.Now().Before(end); {
		select {
		case <-exited:
			msg := logTail()
			os.Remove(logName)
			return nil, fmt.Errorf("%s exited before it listened on $PORT:\n%s", command, msg)
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		default:
		}
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return stop, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	msg := logTail()
	stop()
	return nil, fmt.Errorf("%s did not listen on $PORT (%d) within %s:\n%s", command, port, tryWait, msg)
}

// tryKind: how the task's branch is tried: its repo's try command (which wins), Mako's own build, or not at all.
func (d *Dispatcher) tryKind(r core.Record) (command string, mako, ok bool) {
	if r.Worktree == "" || !exists(r.Worktree) {
		return "", false, false
	}
	if repo := d.repoOf(r); repo != nil && repo.Try != "" {
		return repo.Try, false, true
	}
	if shot.Module(r.Worktree) != "" {
		return "", true, true
	}
	return "", false, false
}

// canTry: «Try this build» is there for the task.
func (d *Dispatcher) canTry(r core.Record) bool {
	if !reviewable(r) {
		return false
	}
	_, _, ok := d.tryKind(r)
	return ok
}

// tried: a copy of the branch at head came up for the task.
func tried(r core.Record, head string) bool {
	return r.SelfCheck != nil && r.SelfCheck.Tried != nil && head != "" && r.SelfCheck.Tried.Commit == head
}

// tryView: the task's copy to try, nil when there is none.
func (d *Dispatcher) tryView(id, head string) *TryView {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tries[id]
	if t == nil {
		return nil
	}
	return &TryView{State: t.state, URL: t.url, Error: t.err, Fresh: head != "" && t.head == head}
}

// TryBuild starts the task's copy to try at the branch's HEAD and opens it on the first step's page; a copy
// already running at that HEAD is only opened again, one of an older HEAD is replaced. It returns once the copy
// is starting: the board follows it through SelfView.
func (d *Dispatcher) TryBuild(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if !reviewable(r) {
		return errors.New("a build can only be tried for Mako agents that are in progress or in an open MR")
	}
	command, mako, ok := d.tryKind(r)
	if !ok {
		return errors.New("this project has no «Try this build»: set its try command in the settings")
	}
	head, err := d.headOf(id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if old := d.tries[id]; old != nil {
		if old.head == head && old.state != "failed" {
			old.seen = time.Now()
			url, running := old.url, old.state == "running"
			d.mu.Unlock()
			if running {
				d.openTry(r, url)
			}
			return nil
		}
		delete(d.tries, id)
		go old.end()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &tryRun{state: "starting", head: head, cancel: cancel, seen: time.Now()}
	d.tries[id] = t
	d.mu.Unlock()
	d.Log(id, "try build starting at "+short(head))
	go func() {
		var url string
		var stop func()
		var err error
		if mako {
			serve := serveMako
			if d.ServeMako != nil {
				serve = d.ServeMako
			}
			url, stop, err = serve(ctx, r.Worktree, d.Config().Home)
		} else {
			var port int
			if port, err = shot.FreePort(); err == nil {
				if stop, err = RunTry(ctx, r.Worktree, command, port); err == nil {
					url = "http://127.0.0.1:" + strconv.Itoa(port)
				}
			}
		}
		d.mu.Lock()
		if d.tries[id] != t { // stopped or replaced meanwhile
			d.mu.Unlock()
			if stop != nil {
				stop()
			}
			return
		}
		if err != nil {
			t.state, t.err = "failed", err.Error()
			d.mu.Unlock()
			d.Log(id, "try build failed: "+firstLine(err.Error()))
			return
		}
		t.state, t.url, t.stop = "running", url, stop
		d.mu.Unlock()
		d.Store.UpdateIfExists(id, func(r *core.Record) {
			if r.SelfCheck == nil {
				r.SelfCheck = &core.SelfCheck{}
			}
			r.SelfCheck.Tried = &core.SelfMark{At: now(), Commit: head}
		})
		d.Log(id, "try build running at "+url)
		d.openTry(r, url)
	}()
	return nil
}

// openTry opens the copy at the page of the first step that names one ("/settings"); the board's main page else.
func (d *Dispatcher) openTry(r core.Record, url string) {
	page := "/"
	for _, s := range d.checkSteps(r) {
		if strings.HasPrefix(s.Open, "/") {
			page = s.Open
			break
		}
	}
	open := notify.Open
	if d.OpenTry != nil {
		open = d.OpenTry
	}
	open(strings.TrimRight(url, "/") + page)
}

// end stops the copy, or its start.
func (t *tryRun) end() {
	t.cancel()
	if t.stop != nil {
		t.stop()
	}
}

// TryStop: «Stop» of the task's copy to try.
func (d *Dispatcher) TryStop(id string) {
	d.mu.Lock()
	t := d.tries[id]
	delete(d.tries, id)
	d.mu.Unlock()
	if t != nil {
		t.end()
		d.Log(id, "try build stopped")
	}
}

// TryKeep: the task's card is open on a board, so its copy to try is kept.
func (d *Dispatcher) TryKeep(id string) {
	d.mu.Lock()
	if t := d.tries[id]; t != nil {
		t.seen = time.Now()
	}
	d.mu.Unlock()
}

// reapTries stops the copies whose card no board has had open for tryIdle (a failed one is only forgotten).
func (d *Dispatcher) reapTries(now time.Time) {
	var idle []string
	d.mu.Lock()
	for id, t := range d.tries {
		if now.Sub(t.seen) > tryIdle {
			idle = append(idle, id)
		}
	}
	d.mu.Unlock()
	for _, id := range idle {
		d.TryStop(id)
	}
}

// StopTries stops every copy to try, as Mako quits.
func (d *Dispatcher) StopTries() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.tries))
	for id := range d.tries {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.TryStop(id)
	}
}
