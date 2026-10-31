// Package update keeps Mako on the latest commit of its own repository: it sees new commits on origin's main
// branch and, when asked, builds them and restarts Mako on the new build. Nothing changes when the build fails.
// The tests are CI's: main is tested on every push, and a flaky test should not hold Mako back.
package update

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// busyPoll: how often the restart looks whether the agents finished their turn.
var busyPoll = 3 * time.Second

// Status is what the board shows.
type Status struct {
	Count  int      `json:"count"`            // commits on origin's main branch the running Mako lacks
	Titles []string `json:"titles,omitempty"` // their subjects, newest first
	// State: "" (nothing under way), "building", "waiting" (for agents to finish their turn), "restarting",
	// "failed".
	State   string `json:"state,omitempty"`
	Busy    int    `json:"busy,omitempty"`    // agents mid-turn, while "waiting"
	Error   string `json:"error,omitempty"`   // in a line; the whole of it goes to Issue
	Issue   string `json:"issue,omitempty"`   // the issue the failure was reported in
	Checked int64  `json:"checked,omitempty"` // ms: the last look at origin
}

// Updater updates the Mako built from Source.
type Updater struct {
	Source   string // the clone Mako is built from
	Revision string // the commit the running Mako is built from; "" → the clone's HEAD
	Exe      string // the running binary
	Args     []string
	// Busy: agents in the middle of a turn; the restart waits until there are none.
	Busy func() int
	// Quit ends Mako; the new one starts once this process is gone.
	Quit func()
	// Build runs in a checkout of the new commit and leaves the new Mako (a binary or Mako.app) at out.
	// nil: `go build` or packaging/mac/build-app.sh.
	Build func(ctx context.Context, tree, out string) error
	// Report opens an issue in Mako's own repository about a failed update and returns its link; nil: none.
	Report func(ctx context.Context, title, body string) (string, error)
	// Restart starts the new Mako once pid has exited; nil: a detached shell that waits for it.
	Restart func(pid int, command []string) error

	mu       sync.Mutex
	status   Status
	busy     bool              // an update is under way
	reported map[string]string // the commit a failure was reported for → the issue
}

// Locate finds the clone Mako was built from: the one the build named, else one around the binary. "" when none.
func Locate(built, exe string) string {
	if built != "" {
		if root := mainCheckout(built); root != "" {
			return root
		}
	}
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return mainCheckout(dir)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}

// mainCheckout: the repository's main working tree (a worktree's too), "" when dir is not in a git repository.
func mainCheckout(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	common, err := gitx.Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	top, err := gitx.Run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return top
}

// BuiltRevision: the commit the running binary is built from, as Go stamped it.
func BuiltRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Titles = append([]string(nil), s.Titles...)
	return s
}

func (u *Updater) set(f func(s *Status)) {
	u.mu.Lock()
	f(&u.status)
	u.mu.Unlock()
}

func (u *Updater) branch() string { return gitx.DetectBaseBranch(u.Source) }

// Check fetches origin's main branch and counts the commits the running Mako lacks.
func (u *Updater) Check(ctx context.Context) error {
	branch := u.branch()
	if _, err := gitx.Run(ctx, u.Source, "fetch", "--quiet", "origin", branch); err != nil {
		return err
	}
	rev := u.Revision
	if rev == "" {
		rev = "HEAD"
	} else if _, err := gitx.Run(ctx, u.Source, "rev-parse", "--verify", "-q", rev+"^{commit}"); err != nil {
		rev = "HEAD" // a commit the clone does not have: compare with what it has checked out
	}
	out, err := gitx.Run(ctx, u.Source, "log", "--format=%s", rev+"..origin/"+branch)
	if err != nil {
		return err
	}
	var titles []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			titles = append(titles, l)
		}
	}
	u.set(func(s *Status) {
		s.Count, s.Titles, s.Checked = len(titles), titles, time.Now().UnixMilli()
		if len(titles) > 20 {
			s.Titles = titles[:20]
		}
	})
	return nil
}

// Watch checks for new commits now and every interval until ctx ends.
func (u *Updater) Watch(ctx context.Context, every time.Duration) {
	for {
		if err := u.Check(ctx); err != nil && ctx.Err() == nil {
			log.Printf("оновлення: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Start updates in the background: false when an update is under way already.
func (u *Updater) Start() bool {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return false
	}
	u.busy = true
	u.status.Error, u.status.Issue, u.status.State = "", "", "building"
	u.mu.Unlock()
	go func() {
		err := u.Apply(context.Background())
		var issue string
		if err != nil {
			log.Printf("оновлення: %v", err)
			issue = u.report(err)
		}
		u.mu.Lock()
		u.busy = false
		if err != nil {
			line, _, _ := strings.Cut(err.Error(), "\n")
			u.status.State, u.status.Error, u.status.Issue = "failed", line, issue
		}
		u.mu.Unlock()
	}()
	return true
}

// report files the failure once per commit of origin's main branch; "" when there is no Report or it failed.
func (u *Updater) report(failure error) string {
	if u.Report == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rev, _ := gitx.Run(ctx, u.Source, "rev-parse", "--short", "origin/"+u.branch())
	u.mu.Lock()
	issue, done := u.reported[rev]
	u.mu.Unlock()
	if done {
		return issue
	}
	line, _, _ := strings.Cut(failure.Error(), "\n")
	title := "Mako не оновився до " + rev + ": " + line
	body := "Оновлення Mako до `" + rev + "` не вдалось, нічого не змінено.\n\n```\n" + failure.Error() + "\n```\n"
	issue, err := u.Report(ctx, title, body)
	if err != nil {
		log.Printf("оновлення: задачу не заведено: %v", err)
		return ""
	}
	u.mu.Lock()
	if u.reported == nil {
		u.reported = map[string]string{}
	}
	u.reported[rev] = issue
	u.mu.Unlock()
	return issue
}

// Apply builds origin's main branch in a checkout of its own, brings the clone up to it, puts the new
// build in place of the running one and restarts Mako once no agent is mid-turn. A failing build changes
// nothing.
func (u *Updater) Apply(ctx context.Context) error {
	branch := u.branch()
	if _, err := gitx.Run(ctx, u.Source, "fetch", "--quiet", "origin", branch); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "mako-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "src")
	if _, err := gitx.Run(ctx, u.Source, "worktree", "add", "--detach", tree, "origin/"+branch); err != nil {
		return err
	}
	defer gitx.Run(context.Background(), u.Source, "worktree", "remove", "--force", tree)

	u.set(func(s *Status) { s.State = "building" })
	bundle := appBundle(u.Exe)
	out := filepath.Join(work, "mako")
	if bundle != "" {
		out = filepath.Join(work, "Mako.app")
	}
	build := u.Build
	if build == nil {
		build = goBuild
	}
	if err := build(ctx, tree, out); err != nil {
		return fmt.Errorf("збірка не вдалась: %w", err)
	}

	// The clone follows, when it is on the main branch (a branch of its own is left alone).
	if cur, _ := gitx.Run(ctx, u.Source, "rev-parse", "--abbrev-ref", "HEAD"); cur == branch {
		if _, err := gitx.Run(ctx, u.Source, "merge", "--ff-only", "--quiet", "origin/"+branch); err != nil {
			return fmt.Errorf("pull у %s: %w", u.Source, err)
		}
	}

	// The agents in Mako end with it: let them finish their turn first.
	for u.Busy != nil {
		n := u.Busy()
		u.set(func(s *Status) { s.State, s.Busy = "waiting", n })
		if n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyPoll):
		}
	}

	u.set(func(s *Status) { s.State = "restarting" })
	command := append([]string{u.Exe}, u.Args...)
	if bundle != "" {
		if err := replace(out, bundle); err != nil {
			return err
		}
		command = []string{"open", bundle}
	} else if err := replace(out, u.Exe); err != nil {
		return err
	}
	restart := u.Restart
	if restart == nil {
		restart = relaunch
	}
	if err := restart(os.Getpid(), command); err != nil {
		return fmt.Errorf("перезапуск: %w", err)
	}
	log.Printf("оновлення: перезапускаюсь")
	if u.Quit != nil {
		u.Quit()
	}
	return nil
}

// appBundle: Mako.app the binary runs in, "" when it runs bare.
func appBundle(exe string) string {
	dir := filepath.Dir(exe) // …/Mako.app/Contents/MacOS
	if filepath.Base(dir) != "MacOS" || filepath.Base(filepath.Dir(dir)) != "Contents" {
		return ""
	}
	if app := filepath.Dir(filepath.Dir(dir)); strings.HasSuffix(app, ".app") {
		return app
	}
	return ""
}

// replace puts src in place of dst: copied next to it first, so a failed copy leaves dst as it was.
func replace(src, dst string) error {
	next, old := dst+".new", dst+".old"
	os.RemoveAll(next)
	os.RemoveAll(old)
	if out, err := exec.Command("cp", "-Rp", src, next).CombinedOutput(); err != nil {
		os.RemoveAll(next)
		return fmt.Errorf("копіювання нової збірки: %s", strings.TrimSpace(string(out)))
	}
	if err := os.Rename(dst, old); err != nil {
		os.RemoveAll(next)
		return err
	}
	if err := os.Rename(next, dst); err != nil {
		os.Rename(old, dst)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// relaunch starts command in a shell of its own that waits for this process to exit first: the new Mako needs
// the board's port.
func relaunch(pid int, command []string) error {
	script := `while kill -0 "$0" 2>/dev/null; do sleep 0.2; done; exec "$@"`
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, strconv.Itoa(pid)}, command...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// goBuild: Mako.app with packaging/mac/build-app.sh when out is a bundle, else the bare binary.
func goBuild(ctx context.Context, tree, out string) error {
	dir := filepath.Join(tree, "go")
	if strings.HasSuffix(out, ".app") {
		if err := run(ctx, dir, "sh", "packaging/mac/build-app.sh"); err != nil {
			return err
		}
		return os.Rename(filepath.Join(dir, "dist", "Mako.app"), out)
	}
	version, _ := gitx.Run(ctx, tree, "describe", "--tags", "--always")
	ldflags := "-X main.version=" + strings.TrimPrefix(version, "v") + " -X 'main.source=" + mainCheckout(tree) + "'"
	return run(ctx, dir, "go", "build", "-ldflags", ldflags, "-o", out, "./cmd/mako")
}

func run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(out.String(), 40))
	}
	return nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
