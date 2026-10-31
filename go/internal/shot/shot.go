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
	return "", errors.New("не знайшов Chrome чи Chromium: встановіть його або вкажіть шлях у CHROME")
}

// Capture photographs url with headless Chrome into out (PNG), the window width×height; wait is how long the
// page's scripts get to load its data.
func Capture(ctx context.Context, chrome, url, out string, width, height int, wait time.Duration) error {
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
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
		"--no-default-browser-check", "--user-data-dir="+profile, fmt.Sprintf("--window-size=%d,%d", window, height),
		fmt.Sprintf("--virtual-time-budget=%d", wait.Milliseconds()), "--screenshot="+out, page)
	msg, err := cmd.CombinedOutput()
	if _, statErr := os.Stat(out); statErr != nil {
		if err == nil {
			err = statErr
		}
		return fmt.Errorf("Chrome не зробив скриншот: %v\n%s", err, tail(string(msg), 800))
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
		return errors.New("скриншот не вдалося обрізати")
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
func CopyHome(home, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"mako.config.json", ".env"} {
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
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/mako")
	cmd.Dir = module
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("збірка Mako з %s: %v\n%s", module, err, tail(string(msg), 2000))
	}
	return nil
}

// Serve runs the Mako at bin read-only with the data in home on port, and returns once its board answers; stop
// ends it.
func Serve(ctx context.Context, bin, home string, port int) (stop func(), err error) {
	cmd := exec.Command(bin, "-home", home, "-readonly", "-no-tray", "-port", fmt.Sprint(port))
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
			if strings.Contains(msg, "flag provided but not defined") {
				return nil, errors.New("ця версія Mako ще не вміє працювати лише для читання (-readonly): скриншот «до» з неї не зробити")
			}
			return nil, fmt.Errorf("Mako не запустився:\n%s", tail(msg, 2000))
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
	return nil, fmt.Errorf("Mako не відповів за 30 с:\n%s", tail(out.String(), 2000))
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
