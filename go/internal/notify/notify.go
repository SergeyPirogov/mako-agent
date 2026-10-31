// Package notify shows system notifications: osascript on macOS, notify-send on Linux.
package notify

import (
	"os/exec"
	"runtime"
	"strconv"
)

// Enabled turns the Mac's notifications off for tests and when the config says so.
var Enabled = true

// Action is a button of a notification: pressing it POSTs to URL (a path of the board), which answers on its own,
// without opening the board.
type Action struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Destructive bool   `json:"destructive,omitempty"`
}

// Notice is one notification. A Tag names it: a later one with the same tag replaces it, and Withdraw takes it away.
type Notice struct {
	Title, Message, URL string
	Tag                 string
	Actions             []Action
}

// Native shows a notification of Mako.app itself (macOS); false when it cannot.
var Native func(n Notice) bool

// NativeWithdraw takes Mako.app's notification with the tag away.
var NativeWithdraw func(tag string)

// Remote also gets every notification: the paired phones' pushes.
var Remote func(n Notice)

// RemoteWithdraw takes the phones' notification with the tag away.
var RemoteWithdraw func(tag string)

// Inform shows a notification; with url, clicking it opens that page where the system allows (terminal-notifier
// on macOS, the «Відкрити» action of notify-send on Linux).
func Inform(title, message, url string) {
	Show(Notice{Title: title, Message: message, URL: url})
}

// Show shows a notification; its actions only where the system lets Mako answer them (Mako.app, the phones).
func Show(n Notice) {
	if Remote != nil {
		go Remote(n)
	}
	if !Enabled {
		return
	}
	title, message, url := n.Title, n.Message, n.URL
	switch runtime.GOOS {
	case "darwin":
		if Native != nil && Native(n) {
			return
		}
		if tn, err := exec.LookPath("terminal-notifier"); err == nil {
			args := []string{"-title", "Mako", "-subtitle", title, "-message", message, "-sound", "default"}
			if url != "" {
				args = append(args, "-open", url)
			}
			if n.Tag != "" {
				args = append(args, "-group", n.Tag)
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

// Withdraw takes away the notification with the tag, wherever it was shown: what it asked is no longer asked.
func Withdraw(tag string) {
	if tag == "" {
		return
	}
	if RemoteWithdraw != nil {
		go RemoteWithdraw(tag)
	}
	if !Enabled || runtime.GOOS != "darwin" {
		return
	}
	if NativeWithdraw != nil {
		NativeWithdraw(tag)
	}
	if tn, err := exec.LookPath("terminal-notifier"); err == nil {
		go exec.Command(tn, "-remove", tag).Run()
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
