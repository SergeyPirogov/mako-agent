//go:build darwin && !cgo

package desktop

import "context"

// Built without cgo there is no menu bar icon on macOS: Mako runs with the board only.
func Available() bool { return false }

func Run(ctx context.Context, app App) { app.Start(ctx) }
