// Package shot takes screenshots of a version of Mako's board for UI tasks: it builds Mako from a worktree (or
// a revision), runs it read-only on a free port with a copy of the current Mako's data, and photographs a page
// with headless Chrome.
package shot

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/proc"
)

// FindChrome: $CHROME, then the usual Chrome, Chromium, Edge and Brave installs and names on PATH.
func FindChrome() (string, error) {
	if c := os.Getenv("CHROME"); c != "" {
		return c, nil
	}
	var paths []string
	if runtime.GOOS == "darwin" {
		for _, app := range []string{"Google Chrome", "Chromium", "Microsoft Edge", "Brave Browser"} {
			paths = append(paths, "/Applications/"+app+".app/Contents/MacOS/"+app)
			if user, err := os.UserHomeDir(); err == nil {
				paths = append(paths, filepath.Join(user, "Applications", app+".app", "Contents", "MacOS", app))
			}
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome", "microsoft-edge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found: install it or set the path in CHROME")
}

// Capture photographs url with headless Chrome into out (PNG), the window width×height; wait is how long the
// page's scripts get to load its data.
// dark: the page is shown as in a system with the dark theme (the board's «auto» theme follows it).
func Capture(ctx context.Context, chrome, url, out string, width, height int, wait time.Duration, dark bool) error {
	profile, err := os.MkdirTemp("", "mako-chrome-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	_ = os.Remove(out) // so a stale file is not taken for the new screenshot
	// Chrome's window is never narrower than 500 px: a phone's width is photographed in a frame of exactly that
	// size, and the picture cut to it.
	page, window := url, width
	if width < minWindow {
		frame := filepath.Join(profile, "phone.html")
		html := fmt.Sprintf(`<!doctype html><body style="margin:0"><iframe src=%q style="display:block;border:0;width:%dpx;height:%dpx"></iframe></body>`,
			url, width, height)
		if err := os.WriteFile(frame, []byte(html), 0o600); err != nil {
			return err
		}
		page, window = "file://"+frame, minWindow
	}
	ctx, cancel := context.WithTimeout(ctx, wait+60*time.Second)
	defer cancel()
	args := []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
		"--no-default-browser-check", "--user-data-dir=" + profile, fmt.Sprintf("--window-size=%d,%d", window, height),
		fmt.Sprintf("--virtual-time-budget=%d", wait.Milliseconds()), "--screenshot=" + out}
	if dark {
		args = append(args, "--force-dark-mode", "--blink-settings=preferredColorScheme=0")
	}
	cmd := proc.Command(ctx, chrome, append(args, page)...)
	msg, err := cmd.CombinedOutput()
	if _, statErr := os.Stat(out); statErr != nil {
		if err == nil {
			err = statErr
		}
		return fmt.Errorf("no screenshot from Chrome: %v\n%s", err, tail(string(msg), 800))
	}
	if window != width {
		return crop(out, width, height)
	}
	return nil
}

// minWindow: the narrowest window headless Chrome makes.
const minWindow = 500

// crop cuts the PNG at path to its top-left width×height.
func crop(path string, width, height int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return errors.New("could not crop the screenshot")
	}
	part := sub.SubImage(image.Rect(0, 0, width, height).Intersect(img.Bounds()))
	f, err = os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, part); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Module: the Go module of Mako in the repository at dir (its go/ folder or the root), "" when it is not Mako.
func Module(dir string) string {
	for _, m := range []string{filepath.Join(dir, "go"), dir} {
		if _, err := os.Stat(filepath.Join(m, "cmd", "mako", "main.go")); err == nil {
			return m
		}
	}
	return ""
}

// CopyHome makes a copy of the Mako home's settings and data in dst, so a second Mako can show them without
// touching the real ones: the config, .env and the state folder, without the log and the paired phones.
func CopyHome(home, dst string, secrets bool) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	names := []string{"mako.config.json"}
	if secrets {
		names = append(names, ".env")
	}
	for _, name := range names {
		if err := copyFile(filepath.Join(home, name), filepath.Join(dst, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	cfg, err := config.Load(home)
	if err != nil && !errors.Is(err, config.ErrMissing) {
		return err
	}
	stateDir := filepath.Join(home, ".mako")
	if cfg != nil && cfg.StateDir != "" {
		stateDir = cfg.StateDir
	}
	into := filepath.Join(dst, ".mako")
	err = filepath.WalkDir(stateDir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if path == stateDir && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		rel, _ := filepath.Rel(stateDir, path)
		switch {
		case rel == "mako.log" || rel == "phones.json" || rel == "screenshots":
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case e.IsDir():
			return os.MkdirAll(filepath.Join(into, rel), 0o700)
		case !e.Type().IsRegular():
			return nil
		}
		return copyFile(path, filepath.Join(into, rel))
	})
	if err != nil {
		return err
	}
	return os.MkdirAll(into, 0o700)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// FreePort: a port on 127.0.0.1 nobody listens on now.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Build compiles Mako from its module into bin.
func Build(ctx context.Context, module, bin string) error {
	cmd := proc.Command(ctx, "go", "build", "-o", bin, "./cmd/mako")
	cmd.Dir = module
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build of Mako from %s: %v\n%s", module, err, tail(string(msg), 2000))
	}
	return nil
}

// Mode: how a copy of Mako runs: Readonly for screenshots (only GET), Try for «Try this build» (the board works
// on the copy of the data, but nothing reaches the trackers, the agents or the repositories).
type Mode string

const (
	Readonly Mode = "-readonly"
	Try      Mode = "-try"
)

// ServeWorktree builds the Mako in the worktree src (or its revision rev, in a temporary worktree) and runs it in
// mode on a free port with a copy of the data in home; it returns its address and what to stop and clean up
// (also when it fails). A copy to try gets no .env: no tokens, so no tracker is reached from it.
func ServeWorktree(ctx context.Context, src, rev, home string, mode Mode) (string, func(), error) {
	top, err := gitOut(src, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, fmt.Errorf("%s is not a git repository: %w", src, err)
	}
	tmp, err := os.MkdirTemp("", "mako-shot-")
	if err != nil {
		return "", nil, err
	}
	var undo []func()
	cleanup := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		os.RemoveAll(tmp)
	}
	repo := top
	if rev != "" {
		repo = filepath.Join(tmp, "src")
		if _, err := gitOut(top, "worktree", "add", "--detach", repo, rev); err != nil {
			return "", cleanup, fmt.Errorf("worktree for %s: %w", rev, err)
		}
		undo = append(undo, func() { gitOut(top, "worktree", "remove", "--force", repo) })
	}
	module := Module(repo)
	if module == "" {
		return "", cleanup, fmt.Errorf("there is no Mako (cmd/mako) in %s: for another project run its build and pass -url", repo)
	}
	bin := filepath.Join(tmp, "mako")
	if err := Build(ctx, module, bin); err != nil {
		return "", cleanup, err
	}
	data := filepath.Join(tmp, "home")
	if err := CopyHome(home, data, mode != Try); err != nil {
		return "", cleanup, fmt.Errorf("copy of Mako data: %w", err)
	}
	port, err := FreePort()
	if err != nil {
		return "", cleanup, err
	}
	stopMako, err := Serve(ctx, bin, data, port, mode)
	if err != nil {
		return "", cleanup, err
	}
	undo = append(undo, stopMako)
	return fmt.Sprintf("http://127.0.0.1:%d", port), cleanup, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// serveArgs: the command line of a copy of Mako in mode.
func serveArgs(home string, port int, mode Mode) []string {
	return []string{"-home", home, string(mode), "-no-tray", "-port", fmt.Sprint(port)}
}

// Serve runs the Mako at bin in mode with the data in home on port, and returns once its board answers; stop
// ends it.
func Serve(ctx context.Context, bin, home string, port int, mode Mode) (stop func(), err error) {
	cmd := exec.Command(bin, serveArgs(home, port, mode)...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	stop = func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/state", port)
	c := http.Client{Timeout: time.Second}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); {
		select {
		case <-exited:
			msg := out.String()
			if strings.Contains(msg, "flag provided but not defined") && mode == Try {
				return nil, errors.New("this version of Mako cannot run as a copy to try (-try) yet: rebase the branch on the main branch")
			}
			if strings.Contains(msg, "flag provided but not defined") {
				return nil, errors.New("this version of Mako cannot run read-only (-readonly) yet: cannot take the \"before\" screenshot from it")
			}
			return nil, fmt.Errorf("the Mako under test failed to start:\n%s", tail(msg, 2000))
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		default:
		}
		if res, err := c.Get(url); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return stop, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	return nil, fmt.Errorf("no answer from Mako within 30 s:\n%s", tail(out.String(), 2000))
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
