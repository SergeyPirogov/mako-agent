package proc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCleanEnv(t *testing.T) {
	got := CleanEnv([]string{"PATH=/bin", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDECODE=1", "CLAUDE_CONFIG_DIR=/c"})
	if strings.Join(got, " ") != "PATH=/bin CLAUDE_CONFIG_DIR=/c" {
		t.Fatal(got)
	}
}

func TestCommandCleansEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("MAKO_PROC_TEST", "kept")
	out, err := Command(context.Background(), "/bin/sh", "-c", `echo "[$CLAUDECODE] $MAKO_PROC_TEST"`).Output()
	if err != nil || strings.TrimSpace(string(out)) != "[] kept" {
		t.Fatalf("%q %v", out, err)
	}
}

// A child that holds the output pipe dies with its parent when the context ends: the call does not wait for it.
func TestCommandKillsGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Command(ctx, "/bin/sh", "-c", "sleep 30 & exec sleep 30").CombinedOutput()
	if err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}
