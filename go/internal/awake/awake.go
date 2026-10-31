// Package awake keeps the Mac from sleeping while an agent works: `caffeinate -i` runs as long as it is asked to.
package awake

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
)

var (
	mu  sync.Mutex
	cmd *exec.Cmd
)

// Keep holds off idle sleep (on) or lets the Mac sleep again (off). Asking for what is already so does
// nothing. `-w` ties caffeinate to Mako's process, so a crashed Mako does not keep the Mac awake.
// Elsewhere than macOS it does nothing.
func Keep(on bool) {
	if runtime.GOOS != "darwin" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if on == (cmd != nil) {
		return
	}
	if !on {
		_ = cmd.Process.Kill()
		cmd = nil
		return
	}
	c := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := c.Start(); err != nil {
		return
	}
	cmd = c
	go func() {
		_ = c.Wait()
		mu.Lock()
		if cmd == c { // it ended by itself: the next Keep(true) starts another
			cmd = nil
		}
		mu.Unlock()
	}()
}

// Active: caffeinate holds the Mac awake now.
func Active() bool {
	mu.Lock()
	defer mu.Unlock()
	return cmd != nil
}
