//go:build unix

package fsx

import (
	"errors"
	"path/filepath"
	"testing"
)

// The second Lock on the same file fails while the first is held, and works once it is released.
func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v, want ErrLocked", err)
	}
	release()
	again, err := Lock(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}
