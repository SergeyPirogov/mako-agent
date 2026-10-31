//go:build !(darwin && cgo)

package desktop

// HasWindow: Mako's own window is macOS only; elsewhere the board opens in the browser.
func HasWindow() bool { return false }

func OpenWindow(url string) {}

func OnReopen(url string) {}

func NotificationsOn(boardURL string) {}

func Notify(title, message, url string) bool { return false }
