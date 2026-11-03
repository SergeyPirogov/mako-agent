package orca

import (
	"context"
	"testing"
	"time"
)

// An Orca call that hangs with a child holding its output: the end of the context ends both.
func TestRunKillsChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, ok := run(ctx, "/bin/sh", t.TempDir(), "-c", "sleep 30 & exec sleep 30"); ok {
		t.Fatal("ok")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}
