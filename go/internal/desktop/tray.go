//go:build !(darwin && !cgo)

package desktop

import (
	"context"
	_ "embed"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

var (
	//go:embed icons/mac.png
	macIcon []byte
	//go:embed icons/mac-attention.png
	macAttention []byte
	//go:embed icons/tray.png
	trayIcon []byte
	//go:embed icons/tray-attention.png
	trayAttention []byte
)

// Available: a menu bar or tray to put the icon in. On Linux that needs a desktop session (DBus).
func Available() bool {
	return runtime.GOOS != "linux" || os.Getenv("DBUS_SESSION_BUS_ADDRESS") != ""
}

// startTimeout: how long Run waits for App.Start to return after the menu is gone.
const startTimeout = 5 * time.Second

// quitHook is what a quit asked for by the system (⌘Q, Quit in the Dock) does: end Run the usual way.
var quitHook atomic.Pointer[context.CancelFunc]

// requestQuit ends Run as «Вийти» does. AppKit would otherwise exit the process at once, with the agents running.
func requestQuit() {
	if f := quitHook.Load(); f != nil {
		(*f)()
	}
}

// waitStart waits for App.Start to return, so nothing is stopped while it still writes the state; false when it
// did not in time.
func waitStart(running *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Run shows the menu and runs Mako until «Вийти», a quit from the system or ctx ends; it returns once App.Start has
// returned (or startTimeout passed). It must be called on the main goroutine.
func Run(ctx context.Context, app App) {
	ctx, cancel := context.WithCancel(ctx)
	quitHook.Store(&cancel)
	defer quitHook.Store(nil)
	go func() {
		<-ctx.Done()
		systray.Quit()
	}()
	var running sync.WaitGroup
	running.Add(1)
	systray.Run(func() { ready(ctx, cancel, app, &running) }, cancel)
	cancel()
	if !waitStart(&running, startTimeout) {
		log.Printf("quit: Mako's loop did not stop in %s", startTimeout)
	}
}

func ready(ctx context.Context, quit context.CancelFunc, app App, running *sync.WaitGroup) {
	normal, attention := trayIcon, trayAttention
	if runtime.GOOS == "darwin" {
		normal, attention = macIcon, macAttention
	}
	systray.SetIcon(normal)
	systray.SetTooltip("Mako")
	if HasWindow() {
		OnReopen(app.BoardURL)
		NotificationsOn(app.BoardURL)
		InterceptQuit()
	}

	headline := systray.AddMenuItem("Mako", "")
	headline.Disable()
	line := systray.AddMenuItem("", "")
	line.Disable()
	newTasks := systray.AddMenuItem("", "")
	var mu sync.Mutex
	ids := make([]string, maxItems)
	slots := make([]*systray.MenuItem, maxItems)
	systray.AddSeparator()
	for i := range slots {
		slots[i] = systray.AddMenuItem("", "")
		slots[i].Hide()
		go func(i int) {
			for range slots[i].ClickedCh {
				mu.Lock()
				id := ids[i]
				mu.Unlock()
				app.Open(app.BoardURL + "/#" + urlEscape(id))
			}
		}(i)
	}
	systray.AddSeparator()
	board := systray.AddMenuItem("", "")
	browser := systray.AddMenuItem("", "")
	if !HasWindow() {
		browser.Hide()
	}
	settings := systray.AddMenuItem("", "")
	poll := systray.AddMenuItem("", "")
	systray.AddSeparator()
	pause := systray.AddMenuItemCheckbox("", "", false)
	login := systray.AddMenuItemCheckbox("", "", StartsAtLogin())
	folder := systray.AddMenuItem("", "")
	restart := systray.AddMenuItemCheckbox("", "", false)
	systray.AddSeparator()
	exit := systray.AddMenuItem("", "")

	// The menu's own words, set once.
	wordsSet := false
	words := func() {
		if wordsSet {
			return
		}
		wordsSet = true
		newTasks.SetTooltip(i18n.T("tray.openBoard"))
		board.SetTitle(i18n.T("tray.openBoard"))
		browser.SetTitle(i18n.T("tray.openBrowser"))
		settings.SetTitle(i18n.T("tray.settings"))
		poll.SetTitle(i18n.T("tray.poll"))
		pause.SetTitle(i18n.T("tray.pause"))
		login.SetTitle(i18n.T("tray.login"))
		folder.SetTitle(i18n.T("tray.folder"))
		restart.SetTitle(i18n.T("tray.restart"))
		exit.SetTitle(i18n.T("tray.quit"))
		Relocalize()
	}

	refresh := func() {
		words()
		v := Snapshot(app.D, time.Now())
		headline.SetTitle(v.Headline)
		line.SetTitle(v.Line)
		if v.NewCount > 0 {
			newTasks.SetTitle(i18n.T("tray.newTasks", v.NewCount))
			newTasks.Show()
		} else {
			newTasks.Hide()
		}
		mu.Lock()
		for i, slot := range slots {
			if i < len(v.Items) {
				ids[i] = v.Items[i].ID
				slot.SetTitle(v.Items[i].Label)
				slot.Show()
			} else {
				ids[i] = ""
				slot.Hide()
			}
		}
		mu.Unlock()
		if app.D.RestartPending() {
			restart.Check()
		} else {
			restart.Uncheck()
		}
		if v.Paused {
			pause.Check()
		} else {
			pause.Uncheck()
		}
		if v.Attention {
			systray.SetIcon(attention)
		} else {
			systray.SetIcon(normal)
		}
		if runtime.GOOS == "darwin" {
			systray.SetTitle(v.Title)
		}
		systray.SetTooltip(v.Headline)
	}
	refresh()

	go func() {
		defer running.Done()
		app.Start(ctx)
	}()
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				refresh()
			case <-newTasks.ClickedCh:
				app.Open(app.BoardURL)
			case <-board.ClickedCh:
				app.Open(app.BoardURL)
			case <-browser.ClickedCh:
				app.OpenBrowser(app.BoardURL)
			case <-settings.ClickedCh:
				app.Open(app.BoardURL + "/settings")
			case <-poll.ClickedCh:
				app.D.Wake()
			case <-pause.ClickedCh:
				app.D.SetPaused(!pause.Checked())
				refresh()
			case <-login.ClickedCh:
				if err := SetStartAtLogin(!login.Checked()); err == nil {
					if StartsAtLogin() {
						login.Check()
					} else {
						login.Uncheck()
					}
				}
			case <-restart.ClickedCh:
				app.D.RequestRestart(!restart.Checked())
				refresh()
			case <-folder.ClickedCh:
				ShowFolder(app.Home)
			case <-exit.ClickedCh:
				quit()
				return
			}
		}
	}()
}
