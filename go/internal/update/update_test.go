package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repos: origin with one commit, a clone of it on main, then two more commits on origin.
func repos(t *testing.T) (clone, built string) {
	t.Helper()
	root := t.TempDir()
	origin, clone := filepath.Join(root, "origin"), filepath.Join(root, "clone")
	os.MkdirAll(origin, 0o755)
	git(t, origin, "init", "-q", "-b", "main")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "first")
	git(t, root, "clone", "-q", origin, clone)
	built = git(t, clone, "rev-parse", "HEAD")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "second")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "third")
	return clone, built
}

func TestCheckCountsNewCommits(t *testing.T) {
	clone, built := repos(t)
	u := &Updater{Source: clone, Revision: built}
	if err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := u.Status()
	if s.Count != 2 || strings.Join(s.Titles, ",") != "third,second" {
		t.Fatalf("status = %+v", s)
	}
}

func TestLocateFindsTheMainCheckout(t *testing.T) {
	clone, _ := repos(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, clone, "worktree", "add", "-q", "--detach", wt)
	want, _ := filepath.EvalSymlinks(clone)
	got, _ := filepath.EvalSymlinks(Locate(wt, "/nowhere/mako"))
	if got != want {
		t.Fatalf("from a worktree: %q, want %q", got, want)
	}
	got, _ = filepath.EvalSymlinks(Locate("/gone", filepath.Join(clone, "go", "mako")))
	if got != want {
		t.Fatalf("around the binary: %q, want %q", got, want)
	}
	if got := Locate("", filepath.Join(t.TempDir(), "mako")); got != "" {
		t.Fatalf("no repo: %q", got)
	}
}

func TestFailingBuildChangesNothing(t *testing.T) {
	clone, built := repos(t)
	exe := filepath.Join(t.TempDir(), "mako")
	os.WriteFile(exe, []byte("old"), 0o755)
	u := &Updater{Source: clone, Revision: built, Exe: exe,
		Build:   func(context.Context, string, string) error { return errors.New("FAIL") },
		Restart: func(int, []string) error { t.Fatal("restarted"); return nil },
	}
	if err := u.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "FAIL") {
		t.Fatalf("err = %v", err)
	}
	if head := git(t, clone, "rev-parse", "HEAD"); head != built {
		t.Fatal("the clone moved")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("the binary changed")
	}
	if out := git(t, clone, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Fatalf("the update's checkout is left: %s", out)
	}
}

func TestApplyInstallsAndRestarts(t *testing.T) {
	busyPoll = time.Millisecond
	clone, built := repos(t)
	exe := filepath.Join(t.TempDir(), "mako")
	os.WriteFile(exe, []byte("old"), 0o755)
	var builds, restarted []string
	busy, quit := 2, false
	u := &Updater{Source: clone, Revision: built, Exe: exe, Args: []string{"-open"},
		Busy: func() int { busy--; return busy },
		Quit: func() { quit = true },
		Build: func(_ context.Context, tree, out string) error {
			builds = append(builds, git(t, tree, "log", "-1", "--format=%s"))
			return os.WriteFile(out, []byte("new"), 0o755)
		},
		Restart: func(_ int, command []string) error { restarted = command; return nil },
	}
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 || builds[0] != "third" {
		t.Fatalf("built %v", builds)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("binary = %q", b)
	}
	if git(t, clone, "log", "-1", "--format=%s") != "third" {
		t.Fatal("the clone was not pulled")
	}
	if strings.Join(restarted, " ") != exe+" -open" || !quit || busy != 0 {
		t.Fatalf("restart %v, quit %v, busy %d", restarted, quit, busy)
	}
}

func TestAppBundle(t *testing.T) {
	if got := appBundle("/Applications/Mako.app/Contents/MacOS/mako"); got != "/Applications/Mako.app" {
		t.Fatal(got)
	}
	if got := appBundle("/Users/me/mako/go/mako"); got != "" {
		t.Fatal(got)
	}
}

// A failed update goes to an issue once per commit; the board gets a line and the link.
func TestFailureReportedOnce(t *testing.T) {
	clone, built := repos(t)
	exe := filepath.Join(t.TempDir(), "mako")
	os.WriteFile(exe, []byte("old"), 0o755)
	var bodies []string
	u := &Updater{Source: clone, Revision: built, Exe: exe,
		Build: func(context.Context, string, string) error {
			return errors.New("go build: exit status 1\nmain.go:1: oops")
		},
		Report: func(_ context.Context, title, body string) (string, error) {
			bodies = append(bodies, body)
			return "https://github.com/me/mako/issues/9", nil
		},
	}
	for i := 0; i < 2; i++ {
		u.Start()
		for deadline := time.Now().Add(10 * time.Second); u.Status().State != "failed"; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("state %q", u.Status().State)
			}
		}
	}
	st := u.Status()
	if st.Error != "build failed: go build: exit status 1" || st.Issue != "https://github.com/me/mako/issues/9" {
		t.Fatalf("status %+v", st)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "main.go:1: oops") {
		t.Fatalf("reported %q", bodies)
	}
}

// An agent keeps working: the update waits; «Скасувати» gives it up with nothing changed, «Перезапустити зараз»
// does not wait.
func TestWaitingCanBeCancelledOrForced(t *testing.T) {
	busyPoll = time.Millisecond
	wait := func(u *Updater, want string) {
		t.Helper()
		for i := 0; i < 500 && u.Status().State != want; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		if u.Status().State != want {
			t.Fatalf("state %q (%s), want %q", u.Status().State, u.Status().Error, want)
		}
	}
	newUpdater := func() (*Updater, *bool, *[]string) {
		clone, built := repos(t)
		exe := filepath.Join(t.TempDir(), "mako")
		os.WriteFile(exe, []byte("old"), 0o755)
		quit, restarted := false, []string{}
		return &Updater{Source: clone, Revision: built, Exe: exe,
			Busy: func() int { return 3 },
			Quit: func() { quit = true },
			Build: func(_ context.Context, _, out string) error {
				return os.WriteFile(out, []byte("new"), 0o755)
			},
			Restart: func(_ int, c []string) error { restarted = c; return nil },
		}, &quit, &restarted
	}

	u, quit, _ := newUpdater()
	u.Start()
	wait(u, "waiting")
	if !u.Waiting() || u.Status().Busy != 3 {
		t.Fatalf("status %+v", u.Status())
	}
	if !u.Cancel() {
		t.Fatal("not cancelled")
	}
	wait(u, "")
	if u.Waiting() || *quit {
		t.Fatal("cancelled update restarted or still holds")
	}
	if b, _ := os.ReadFile(u.Exe); string(b) != "old" {
		t.Fatal("the binary changed")
	}
	if !u.Start() { // can be started again
		t.Fatal("no new start")
	}
	u.Cancel()
	wait(u, "")

	u, quit, restarted := newUpdater()
	u.Start()
	wait(u, "waiting")
	if !u.Force() {
		t.Fatal("not forced")
	}
	for i := 0; i < 500 && !*quit; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !*quit || len(*restarted) == 0 {
		t.Fatalf("no restart: quit %v, %v", *quit, *restarted)
	}
}
