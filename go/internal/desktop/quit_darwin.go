//go:build darwin && cgo

package desktop

/*
// Declarations only: with //export the preamble is copied into two files, and window_darwin.go's definitions would clash.
*/
import "C"

// makoQuitRequested is called by the app delegate (window_darwin.go) on a quit from the system.
//
//export makoQuitRequested
func makoQuitRequested() { requestQuit() }
