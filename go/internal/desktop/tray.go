//go:build !(darwin && !cgo)

package desktop

import (
	"context"
	_ "embed"
	"os"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
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

// Run shows the menu and runs Mako until «Вийти» or ctx ends. It must be called on the main goroutine.
func Run(ctx context.Context, app App) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-ctx.Done()
		systray.Quit()
	}()
	systray.Run(func() { ready(ctx, cancel, app) }, cancel)
}

func ready(ctx context.Context, quit context.CancelFunc, app App) {
	normal, attention := trayIcon, trayAttention
	if runtime.GOOS == "darwin" {
		normal, attention = macIcon, macAttention
	}
	systray.SetIcon(normal)
	systray.SetTooltip("Mako")

	headline := systray.AddMenuItem("Mako", "")
	headline.Disable()
	line := systray.AddMenuItem("", "")
	line.Disable()
	newTasks := systray.AddMenuItem("", "Відкрити дошку")
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
	board := systray.AddMenuItem("Відкрити дошку", "")
	browser := systray.AddMenuItem("Відкрити в браузері", "")
	if !HasWindow() {
		browser.Hide()
	}
	settings := systray.AddMenuItem("Налаштування…", "")
	poll := systray.AddMenuItem("Перевірити трекери зараз", "")
	systray.AddSeparator()
	pause := systray.AddMenuItemCheckbox("Пауза: не шукати нових задач", "", false)
	login := systray.AddMenuItemCheckbox("Запускати при вході в систему", "", StartsAtLogin())
	folder := systray.AddMenuItem("Показати папку даних", "")
	systray.AddSeparator()
	exit := systray.AddMenuItem("Вийти з Mako", "")

	refresh := func() {
		v := Snapshot(app.D, time.Now())
		headline.SetTitle(v.Headline)
		line.SetTitle(v.Line)
		if v.NewCount > 0 {
			newTasks.SetTitle("Нові задачі: " + itoa(v.NewCount))
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

	go app.Start(ctx)
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
			case <-folder.ClickedCh:
				ShowFolder(app.Home)
			case <-exit.ClickedCh:
				quit()
				return
			}
		}
	}()
}
