//go:build !(darwin && !cgo)

package desktop

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRequestQuit(t *testing.T) {
	quitHook.Store(nil)
	requestQuit() // no menu running: nothing to end, and no panic

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quitHook.Store(&cancel)
	defer quitHook.Store(nil)
	requestQuit()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("a quit from the system did not end Mako's context")
	}
}

func TestWaitStart(t *testing.T) {
	var running sync.WaitGroup
	if !waitStart(&running, time.Second) {
		t.Fatal("nothing runs: no waiting")
	}

	// Start still finishing its round: waited for.
	var wrote bool
	running.Add(1)
	go func() {
		defer running.Done()
		time.Sleep(50 * time.Millisecond)
		wrote = true
	}()
	if !waitStart(&running, 2*time.Second) || !wrote {
		t.Fatalf("did not wait for Start: wrote=%v", wrote)
	}

	// Start stuck: the quit is not held forever.
	stuck := make(chan struct{})
	defer close(stuck)
	running.Add(1)
	go func() {
		defer running.Done()
		<-stuck
	}()
	start := time.Now()
	if waitStart(&running, 50*time.Millisecond) {
		t.Fatal("Start has not returned, but waitStart said so")
	}
	if time.Since(start) > time.Second {
		t.Fatal("waitStart overstayed its timeout")
	}
}
