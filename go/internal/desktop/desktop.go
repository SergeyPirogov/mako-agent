// Package desktop puts Mako in the menu bar (the tray on Linux and Windows): what is new and in Orca, the
// board, settings, pause, start at login, quit.
package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
)

// App is what the menu shows and acts on.
type App struct {
	D        *dispatch.Dispatcher
	BoardURL string
	Home     string
	Open     func(url string) // the board: Mako's window on macOS, else the browser
	// OpenBrowser: the same pages in the browser, for whoever prefers a tab.
	OpenBrowser func(url string)
	// Start runs Mako's loop until ctx ends; the menu starts it once it is up.
	Start func(ctx context.Context)
}

// Item is one task line in the menu.
type Item struct {
	ID    string
	Label string
}

// View is the menu's content at a moment.
type View struct {
	Headline  string
	Line      string // the last search, the pause or what is wrong
	Title     string // next to the icon on macOS: how many new tasks
	Attention bool
	NewCount  int
	Items     []Item
	Paused    bool
}

const maxItems = 8

// Snapshot builds the menu from the dispatcher's state.
func Snapshot(d *dispatch.Dispatcher, now time.Time) View {
	problems, paused := d.Status()
	cfg := d.Config()
	var v View
	v.Paused = paused
	doing := 0
	var news, active []core.Record
	for _, r := range d.Store.All() {
		if r.ArchivedAt != 0 {
			continue
		}
		switch r.Column(now.UnixMilli()) {
		case "new":
			if !r.Postponed(now.UnixMilli()) {
				news = append(news, r)
			}
		case "doing":
			doing++
			active = append(active, r)
		case "review":
			active = append(active, r)
		}
	}
	v.NewCount = len(news)
	if cfg.WipLimit > 0 {
		v.Headline = fmt.Sprintf("Mako · в Orca %d з %d", doing, cfg.WipLimit)
	} else {
		v.Headline = fmt.Sprintf("Mako · в Orca %d", doing)
	}
	switch {
	case len(problems) > 0:
		v.Line = "⚠ " + problems[0]
	case paused:
		v.Line = "⏸ Пошук задач на паузі"
	default:
		var last time.Time
		failed := ""
		for id, s := range d.Searches() {
			if s.At.After(last) {
				last = s.At
			}
			if s.Error != "" {
				failed = id + ": " + s.Error
			}
		}
		switch {
		case failed != "":
			v.Line = "⚠ " + failed
		case last.IsZero():
			v.Line = "Шукаю задачі…"
		default:
			v.Line = "Перевірено о " + last.Format("15:04")
		}
	}
	v.Attention = v.NewCount > 0 || len(problems) > 0
	if v.NewCount > 0 {
		v.Title = fmt.Sprint(v.NewCount)
	}
	// An agent waiting for you comes first and lights the icon.
	agents := d.Agents()
	waiting := 0
	sort.SliceStable(active, func(i, j int) bool {
		return agents[active[i].ID].State == "waiting" && agents[active[j].ID].State != "waiting"
	})
	for _, r := range append(news, active...) {
		status := map[string]string{"new": "нова", "doing": "в Orca", "review": "на рев'ю"}[r.Column(now.UnixMilli())]
		mark := ""
		switch {
		case r.Status == core.Queued:
			status = "у черзі"
		case agents[r.ID].State == "waiting":
			status, mark = "чекає на вас", "● "
			waiting++
		case agents[r.ID].State == "done":
			status = "агент закінчив"
		case agents[r.ID].State == "stopped":
			status = "агент зупинився"
		}
		if len(v.Items) < maxItems {
			v.Items = append(v.Items, Item{ID: r.ID, Label: fmt.Sprintf("%s%s  %s — %s", mark, r.ID, clip(r.Task.Title, 40), status)})
		}
	}
	if waiting > 0 {
		v.Attention = true
		v.Title = strings.TrimSpace("✋ " + v.Title)
	}
	return v
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// --- start at login

func loginFile() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "dev.mako.agent.plist")
	}
	return filepath.Join(home, ".config", "autostart", "mako.desktop")
}

// StartsAtLogin: Mako has a launch agent (macOS) or an autostart entry (Linux).
func StartsAtLogin() bool {
	_, err := os.Stat(loginFile())
	return err == nil
}

// SetStartAtLogin adds or removes the entry that starts this binary at login.
func SetStartAtLogin(on bool) error {
	path := loginFile()
	if !on {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	var content string
	if runtime.GOOS == "darwin" {
		content = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.mako.agent</string>
  <key>ProgramArguments</key><array><string>` + xmlEscape(exe) + `</string></array>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`
	} else {
		content = "[Desktop Entry]\nType=Application\nName=Mako\nExec=\"" + exe + "\"\nX-GNOME-Autostart-enabled=true\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ShowFolder opens the data folder in Finder or the file manager.
func ShowFolder(path string) {
	if runtime.GOOS == "darwin" {
		exec.Command("open", path).Start()
		return
	}
	exec.Command("xdg-open", path).Start()
}

// OpenedAsApp: started from Finder, Launchpad, the Dock or `open`, not from a terminal or at login. Launch Services
// names the process after the app's bundle id; launchd names a launch agent's after its label (dev.mako.agent).
func OpenedAsApp() bool {
	return runtime.GOOS == "darwin" && strings.HasPrefix(os.Getenv("XPC_SERVICE_NAME"), "application.dev.mako.desktop.")
}

// FixPath: an app started from Finder gets a minimal PATH; take the login shell's, so git, orca, claude and gh
// are found.
func FixPath() {
	if runtime.GOOS != "darwin" || os.Getenv("TERM") != "" {
		return
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-ilc", "printf '__PATH__%s' \"$PATH\"").Output()
	if err != nil {
		return
	}
	if i := strings.LastIndex(string(out), "__PATH__"); i >= 0 {
		if p := strings.TrimSpace(string(out[i+8:])); p != "" {
			os.Setenv("PATH", p)
		}
	}
}
