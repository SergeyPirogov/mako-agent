//go:build unix

package main

import (
	"strings"
	"testing"
)

// A second Mako on the same state folder (another port, the same MAKO_HOME) does not start: it would write over
// the first one's state.json.
func TestLockStateOnePerFolder(t *testing.T) {
	dir := t.TempDir()
	release, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockState(dir); err == nil || !strings.Contains(err.Error(), "another Mako already uses "+dir) {
		t.Fatalf("second Mako: %v", err)
	}
	release()
	again, err := lockState(dir)
	if err != nil {
		t.Fatalf("after the first one stopped: %v", err)
	}
	again()
}
