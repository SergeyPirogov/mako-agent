// Mako: watches GitLab, GitHub and Jira for tasks assigned to you, hands the ones you take to Orca, and moves
// their issues along: in progress, in review once the MR is open, done after the merge.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/awake"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/desktop"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
	"github.com/SergeyPirogov/mako-agent/go/internal/setup"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/update"
	"github.com/SergeyPirogov/mako-agent/go/internal/web"
)

var version = "dev"

// source: the clone this Mako is built from (build-app.sh names it), for updating itself.
var source = ""

// The menu bar (AppKit) must run on the main thread: keep the main goroutine on it.
func init() { runtime.LockOSThread() }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "screenshot" {
		if err := screenshot(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "mako screenshot:", err)
			os.Exit(1)
		}
		return
	}
	homeFlag := flag.String("home", "", "folder with mako.config.json and .env (default MAKO_HOME, ~/.mako or the Mako app folder)")
	open := flag.Bool("open", false, "open the board in the browser")
	noTray := flag.Bool("no-tray", false, "no menu bar icon: only the board in the browser")
	readonly := flag.Bool("readonly", false, "only show the board (for screenshots): no task polling, agents or changes")
	try := flag.Bool("try", false, "a copy to try a branch on («Try this build»): the board works on the copy of the data in -home, nothing reaches the trackers, the agents or the repositories")
	port := flag.Int("port", 0, "board port instead of the configured one")
	flag.Parse()
	if *readonly || *try {
		*noTray, *open = true, false
	}
	if err := run(*homeFlag, *open, *noTray, *readonly, *try, *port); err != nil {
		fmt.Fprintln(os.Stderr, "mako:", err)
		os.Exit(1)
	}
}

// maxLog: past it mako.log goes to mako.log.1 at the start.
const maxLog = 10 << 20

// lockState takes the lock of the state folder for as long as Mako runs.
func lockState(dir string) (release func(), err error) {
	release, err = fsx.Lock(filepath.Join(dir, "lock"))
	if errors.Is(err, fsx.ErrLocked) {
		return nil, fmt.Errorf("another Mako already uses %s", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "lock"), err)
	}
	return release, nil
}

// showExisting: Mako is already running at addr, so its board is shown (in its window) instead of starting a
// second one. false when nothing answers there.
func showExisting(addr string) bool {
	if !running("http://" + addr) {
		return false
	}
	if !showRunning("http://" + addr) {
		notify.Open("http://" + addr)
	}
	return true
}

// reportBrokenState puts a state.json that could not be read among Mako's errors, so it is seen on the board
// and not only in the log.
func reportBrokenState(d *dispatch.Dispatcher, store *state.Store) {
	if store.Broken != "" {
		d.Fault("mako", "state.json broken", fmt.Errorf("it could not be read and is kept as %s; Mako started with an empty board", store.Broken))
	}
}

func run(homeFlag string, open, noTray, readonly, try bool, port int) error {
	desktop.FixPath()
	home := findHome(homeFlag)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	cfg, ts, problems := setup.Build(home)
	// A copy: for screenshots, or to try a branch on.
	copied := readonly || try
	if copied {
		// A Mako for screenshots runs on a copy of the data in home: never on the state of the real one.
		cfg.StateDir = filepath.Join(home, ".mako")
		cfg.Notifications.Enabled = false
	}
	if port > 0 {
		cfg.Notifications.Port = port
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Notifications.Port)
	// One Mako per state folder: a second one (on another port too) would write over the same state.json and
	// rotate the first one's log. Taken before anything in the folder is touched.
	release, err := lockState(cfg.StateDir)
	if err != nil {
		if !copied && showExisting(addr) {
			return nil
		}
		return err
	}
	defer release()
	// Started from Finder nobody sees stderr: the log goes to a file too, the older part of a big one to mako.log.1.
	logFile := filepath.Join(cfg.StateDir, "mako.log")
	if err := fsx.Rotate(logFile, maxLog); err != nil {
		fmt.Fprintln(os.Stderr, "mako: mako.log not rotated:", err)
	}
	if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer f.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	for _, p := range problems {
		log.Printf("settings: %s", p)
	}
	notify.Enabled = cfg.Notifications.Enabled

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if !copied && showExisting(addr) {
			return nil
		}
		return fmt.Errorf("port %d is busy: %w", cfg.Notifications.Port, err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state.json: %w", err)
	}
	d := dispatch.New(cfg, store, ts)
	reportBrokenState(d, store)
	d.Problems = problems
	d.Version = version
	d.BoardURL = "http://" + ln.Addr().String()
	defer d.StopTries() // the copies to try of the tasks' branches go with Mako
	// The Mac does not fall asleep while an agent works, unless the settings let it.
	d.KeepAwake = func(on bool) { awake.Keep(on && d.Config().KeepAwake) }
	// The board opens in Mako's own window on macOS (it needs the menu bar's run loop), else in the browser.
	tray := !noTray && desktop.Available()
	show := notify.Open
	if tray && desktop.HasWindow() {
		show = desktop.OpenWindow
		notify.Native = desktop.Notify // Mako.app's own notifications, once macOS allows them
		notify.NativeWithdraw = desktop.Withdraw
	}
	phones, err := phone.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	if readonly {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serveReadonly(ctx, ln, &web.Server{D: d, Version: version, Home: home, Show: func(string) {}, Phones: phones})
	}
	if try {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serveTry(ctx, ln, &web.Server{D: d, Version: version, Home: home, Show: func(string) {}, Try: true})
	}
	// Everything the Mac is told goes to the paired phones too, with board links made relative to the app.
	push := func(m phone.Message) {
		for _, err := range phones.Push(context.Background(), m) {
			log.Printf("phone: %v", err)
		}
	}
	notify.Remote = func(n notify.Notice) {
		link := strings.TrimPrefix(n.URL, d.BoardURL)
		if link == "" {
			link = "/"
		}
		m := phone.Message{Title: n.Title, Body: n.Message, URL: link, Tag: n.Tag}
		for _, a := range n.Actions {
			m.Actions = append(m.Actions, phone.Action{Title: a.Title, URL: a.URL})
		}
		push(m)
	}
	// The kinds turned off in the settings, as they are now: a saved change applies at once.
	notify.Muted = func(kind string) bool { return d.Config().Notifications.Muted(kind) }
	notify.RemoteWithdraw = func(tag string) {
		push(phone.Message{Title: "Mako", Body: "This request was already answered", Tag: tag, Close: true})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, quit := context.WithCancel(ctx)
	defer quit()
	// «Перезапустити, коли агенти закінчать хід»: once none works, Mako quits as usual and starts again.
	var restart atomic.Bool
	go func() {
		if d.WaitRestart(ctx, 2*time.Second) {
			restart.Store(true)
			quit()
		}
	}()
	// Mako built from a clone of its repository updates itself from it.
	var updater *update.Updater
	if exe, err := os.Executable(); err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
		orca.MakoCommand = orca.ShellQuote(exe) // agents take screenshots of UI changes with it
		if src := update.Locate(source, exe); src != "" {
			d.Source = src
			updater = &update.Updater{Source: src, Revision: update.BuiltRevision(), Exe: exe, Args: os.Args[1:], Busy: d.Busy, Quit: quit,
				Report: func(ctx context.Context, title, body string) (string, error) {
					return reportToMako(ctx, d, src, title, body)
				}}
			go updater.Watch(ctx, 30*time.Minute)
			// While the update waits for the agents, no new one starts: it would keep it waiting.
			d.Hold = func() string {
				if updater.Waiting() {
					return "starts after the Mako update"
				}
				return ""
			}
		}
	}
	// Mako installed from a release has no clone: it only tells that a newer release is out.
	var releases *update.Releases
	if updater == nil && update.IsRelease(version) {
		releases = &update.Releases{Current: version, File: filepath.Join(cfg.StateDir, "release.json")}
		go releases.Watch(ctx, 6*time.Hour)
	}
	ws := &web.Server{D: d, Version: version, Home: home, Show: show, Phones: phones, Update: updater, Release: releases}
	// The relay brings the phone to this Mac from any network, without Tailscale (docs/relay.md).
	if addr := relay.Addr(); addr != "" {
		if token, err := relay.LoadToken(cfg.StateDir); err != nil {
			log.Printf("relay: %v", err)
		} else {
			rc := &relay.Client{Addr: addr, Token: token, Local: d.BoardURL}
			ws.Relay = rc.URL
			go rc.Run(ctx)
		}
	}
	srv := &http.Server{Handler: ws.Handler()}
	go srv.Serve(ln)
	log.Printf("Mako %s: %s, board %s", version, home, d.BoardURL)
	if len(cfg.Trackers) == 0 {
		show(d.BoardURL + "/settings") // first start: straight to the settings
	} else if open || (tray && desktop.HasWindow() && desktop.OpenedAsApp()) {
		show(d.BoardURL) // opened as an app: show the board, as the menu bar icon may be hidden
	}

	if !tray {
		d.Run(ctx)
	} else {
		desktop.Run(ctx, desktop.App{D: d, BoardURL: d.BoardURL, Home: home, Open: show, OpenBrowser: notify.Open, Start: d.Run})
	}
	d.Sessions.StopAll() // the agents in Mako's terminal and chat end with it
	d.Chats.StopAll()
	awake.Keep(false)
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	if !restart.Load() {
		return err
	}
	// The agents get to save their conversations before the new Mako resumes them.
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end) && d.Sessions.Live()+d.Chats.Live() > 0; {
		time.Sleep(100 * time.Millisecond)
	}
	return relaunch()
}

// serveReadonly shows the board until Mako is told to stop: no polling, no agents, and nothing but GET
// requests, so a screenshot never changes a task or the tracker.
func serveReadonly(ctx context.Context, ln net.Listener, s *web.Server) error {
	h := s.Handler()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	log.Printf("Mako %s read-only: http://%s", version, ln.Addr())
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// serveTry serves the board of a copy to try a branch on: no polling and no agents (the dispatcher's loop does
// not run), and of the changes only those that stay in the copy's data (tryAllowed); the rest is refused, for
// it would reach the real trackers, agents or repositories the copied settings point at.
func serveTry(ctx context.Context, ln net.Listener, s *web.Server) error {
	srv := &http.Server{Handler: tryOnly(s.Handler())}
	go srv.Serve(ln)
	log.Printf("Mako %s to try: http://%s", version, ln.Addr())
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// tryOnly: h with only the requests a copy to try may take.
func tryOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tryAllowed(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"ok":false,"error":"not in a build to try: it would reach the trackers, the agents or the repositories"}`)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// tryChanges: what a copy to try may change: its own settings and its tasks' marks in its state.json.
var tryChanges = map[string]bool{
	"/api/settings": true, "/api/pause": true, "/api/tracker-pause": true, "/api/release/dismiss": true,
	"/api/selfcheck": true, "/api/selfstep": true, "/api/selfskip": true, "/api/later": true,
}

func tryAllowed(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost && tryChanges[r.URL.Path]
}

// relaunch replaces Mako with the binary at its path, which may be a new build: the same pid, arguments and
// environment.
func relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	log.Printf("restart: %s", exe)
	return fmt.Errorf("restart: %w", syscall.Exec(exe, os.Args, os.Environ()))
}

func running(base string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	res, err := c.Get(base + "/api/state")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

// showRunning asks the running Mako to show its board.
func showRunning(base string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("POST", base+"/api/show", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Mako", "1") // the board takes a POST without Origin from Mako only
	res, err := c.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

// findHome: -home, MAKO_HOME, ~/.mako when it has a config (the command-line setup), else the desktop app's
// folder (~/Library/Application Support/Mako on macOS), the same places the TypeScript Mako uses.
func findHome(flagValue string) string {
	if flagValue != "" {
		return config.Expand(flagValue)
	}
	if h := os.Getenv("MAKO_HOME"); h != "" {
		return config.Expand(h)
	}
	user, _ := os.UserHomeDir()
	cli := filepath.Join(user, ".mako")
	if _, err := os.Stat(filepath.Join(cli, "mako.config.json")); err == nil {
		return cli
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(user, "Library", "Application Support", "Mako")
	}
	return filepath.Join(user, ".config", "Mako")
}

// reportToMako opens an issue in Mako's own repository (origin of its clone), through the tracker of that host.
func reportToMako(ctx context.Context, d *dispatch.Dispatcher, src, title, body string) (string, error) {
	origin, _ := gitx.OriginURL(ctx, src)
	project := gitx.RemoteProject(origin)
	for _, t := range d.TrackerList() {
		c, ok := t.(trackers.Creator)
		if host := dispatch.TrackerHost(t); !ok || host == "" || !strings.Contains(origin, host) {
			continue
		}
		task, err := c.Create(ctx, trackers.NewIssue{Project: project, Title: title, Description: body, Bug: true})
		if err != nil {
			return "", err
		}
		return task.URL, nil
	}
	return "", fmt.Errorf("no tracker for %s", origin)
}
