// Mako: watches GitLab, GitHub and Jira for tasks assigned to you, hands the ones you take to Orca, and moves
// their issues along: in progress, in review once the MR is open, done after the merge.
package main

import (
	"context"
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
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/desktop"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/setup"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/web"
)

var version = "dev"

// The menu bar (AppKit) must run on the main thread: keep the main goroutine on it.
func init() { runtime.LockOSThread() }

func main() {
	homeFlag := flag.String("home", "", "папка з mako.config.json і .env (типово MAKO_HOME, ~/.mako або папка застосунку Mako)")
	open := flag.Bool("open", false, "відкрити дошку в браузері")
	noTray := flag.Bool("no-tray", false, "без значка в рядку меню: лише дошка в браузері")
	flag.Parse()
	if err := run(*homeFlag, *open, *noTray); err != nil {
		fmt.Fprintln(os.Stderr, "mako:", err)
		os.Exit(1)
	}
}

func run(homeFlag string, open, noTray bool) error {
	desktop.FixPath()
	home := findHome(homeFlag)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	cfg, ts, problems := setup.Build(home)
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	// Started from Finder nobody sees stderr: the log goes to a file too.
	if f, err := os.OpenFile(filepath.Join(cfg.StateDir, "mako.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		defer f.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	for _, p := range problems {
		log.Printf("налаштування: %s", p)
	}
	notify.Enabled = cfg.Notifications.Enabled

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Notifications.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Mako is already running: show its board instead of starting a second one.
		if running("http://" + addr) {
			notify.Open("http://" + addr)
			return nil
		}
		return fmt.Errorf("порт %d зайнятий: %w", cfg.Notifications.Port, err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state.json: %w", err)
	}
	d := dispatch.New(cfg, store, ts)
	d.Problems = problems
	d.BoardURL = "http://" + ln.Addr().String()
	srv := &http.Server{Handler: (&web.Server{D: d, Version: version, Home: home}).Handler()}
	go srv.Serve(ln)
	log.Printf("Mako %s: %s, дошка %s", version, home, d.BoardURL)
	if len(cfg.Trackers) == 0 {
		notify.Open(d.BoardURL + "/settings") // first start: straight to the settings
	} else if open {
		notify.Open(d.BoardURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if noTray || !desktop.Available() {
		d.Run(ctx)
	} else {
		desktop.Run(ctx, desktop.App{D: d, BoardURL: d.BoardURL, Home: home, Open: notify.Open, Start: d.Run})
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
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
