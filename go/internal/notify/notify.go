// Package notify shows system notifications: osascript on macOS, notify-send on Linux.
package notify

import (
	"os/exec"
	"runtime"
	"strconv"
)

// Enabled turns notifications off for tests and when the config says so.
var Enabled = true

// Inform shows a notification; with url, clicking it opens that page where the system allows (terminal-notifier
// on macOS, the «Відкрити» action of notify-send on Linux).
func Inform(title, message, url string) {
	if !Enabled {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		if tn, err := exec.LookPath("terminal-notifier"); err == nil {
			args := []string{"-title", "Mako", "-subtitle", title, "-message", message, "-sound", "default"}
			if url != "" {
				args = append(args, "-open", url)
			}
			go exec.Command(tn, args...).Run()
			return
		}
		script := "display notification " + strconv.Quote(message) + " with title \"Mako\" subtitle " + strconv.Quote(title) + " sound name \"default\""
		go exec.Command("osascript", "-e", script).Run()
	case "linux":
		if url == "" {
			go exec.Command("notify-send", "--app-name", "Mako", title, message).Run()
			return
		}
		go func() {
			out, _ := exec.Command("notify-send", "--app-name", "Mako", "--wait", "--action", "open=Відкрити", title, message).Output()
			if string(out) == "open\n" {
				Open(url)
			}
		}()
	}
}

// Open opens a page in the default browser.
func Open(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "windows":
		exec.Command("cmd", "/c", "start", "", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}
