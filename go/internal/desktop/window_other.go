//go:build !(darwin && cgo)

package desktop

import "github.com/SergeyPirogov/mako-agent/go/internal/notify"

// HasWindow: Mako's own window is macOS only; elsewhere the board opens in the browser.
func HasWindow() bool { return false }

func OpenWindow(url string) {}

func OnReopen(url string) {}

func NotificationsOn(boardURL string) {}

func Notify(n notify.Notice) bool { return false }

func Withdraw(tag string) {}

func Relocalize() {}
