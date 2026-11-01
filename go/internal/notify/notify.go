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
	// Kind: what the notification is about (one of the Kinds), so it can be turned off in the settings.
	Kind string
}

// Kinds of notifications one can turn off in the settings. The morning digest has its own switch.
const (
	NewTask     = "newTask"     // a new task appeared in a tracker
	Waiting     = "waiting"     // an agent waits for an answer
	Finished    = "finished"    // an agent finished its turn
	Stopped     = "stopped"     // an agent stopped, a hand-off failed or was abandoned
	Review      = "review"      // a merge request appeared
	Merged      = "merged"      // a merge request was merged
	AgentReview = "agentReview" // the reviewer agent passed a branch or left remarks
	Reminder    = "reminder"    // a snoozed task is back
	Gone        = "gone"        // a task in work was closed or deleted in the tracker
	Limits      = "limits"      // the queue paused at the Claude limit or went on
	Secrets     = "secrets"     // a push was stopped: the changes hold secrets
	ReviewAsked = "reviewAsked" // I was made a reviewer of someone's merge request
)

// Muted says the kind is turned off: such notifications are not shown, nor pushed to the phones.
var Muted func(kind string) bool

// Native shows a notification of Mako.app itself (macOS); false when it cannot.
var Native func(n Notice) bool

// NativeWithdraw takes Mako.app's notification with the tag away.
var NativeWithdraw func(tag string)

// Remote also gets every notification: the paired phones' pushes.
var Remote func(n Notice)

// RemoteWithdraw takes the phones' notification with the tag away.
var RemoteWithdraw func(tag string)

// Inform shows a notification of the kind; with url, clicking it opens that page where the system allows (terminal-notifier
// on macOS, the «Відкрити» action of notify-send on Linux).
func Inform(kind, title, message, url string) {
	Show(Notice{Kind: kind, Title: title, Message: message, URL: url})
}

// Show shows a notification; its actions only where the system lets Mako answer them (Mako.app, the phones).
func Show(n Notice) {
	if n.Kind != "" && Muted != nil && Muted(n.Kind) {
		return
	}
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
