// Mako: watches GitLab, GitHub and Jira for tasks assigned to you, hands the ones you take to Orca, and moves
// their issues along: in progress, in review once the MR is open, done after the merge.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/web"
)

var version = "dev"

func main() {
	homeFlag := flag.String("home", "", "папка з mako.config.json і .env (типово MAKO_HOME, ~/.mako або папка застосунку Mako)")
	open := flag.Bool("open", false, "відкрити дошку в браузері")
	flag.Parse()
	if err := run(*homeFlag, *open); err != nil {
		fmt.Fprintln(os.Stderr, "mako:", err)
		os.Exit(1)
	}
}

func run(homeFlag string, open bool) error {
	home := findHome(homeFlag)
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	notify.Enabled = cfg.Notifications.Enabled
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state.json: %w", err)
	}
	var ts []trackers.Tracker
	for _, tc := range cfg.Trackers {
		t, err := trackers.New(tc, cfg)
		if err != nil {
			log.Printf("[%s] вимкнено: %v", tc.ID, err)
			continue
		}
		ts = append(ts, t)
	}
	if len(ts) == 0 {
		return errors.New("немає жодного робочого трекера: додайте GitLab, GitHub або Jira в " + filepath.Join(home, "mako.config.json"))
	}

	d := dispatch.New(cfg, store, ts)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Notifications.Port))
	if err != nil {
		return fmt.Errorf("порт %d зайнятий (TypeScript-версія Mako ще запущена?): %w", cfg.Notifications.Port, err)
	}
	d.BoardURL = "http://" + ln.Addr().String()
	srv := &http.Server{Handler: (&web.Server{D: d, Version: version}).Handler()}
	go srv.Serve(ln)
	log.Printf("Mako %s: %s, дошка %s", version, home, d.BoardURL)
	if open {
		notify.Open(d.BoardURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d.Run(ctx)
	return srv.Shutdown(context.Background())
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
	app := filepath.Join(user, ".config", "Mako")
	if runtime.GOOS == "darwin" {
		app = filepath.Join(user, "Library", "Application Support", "Mako")
	}
	if _, err := os.Stat(filepath.Join(app, "mako.config.json")); err == nil {
		return app
	}
	return cli
}
