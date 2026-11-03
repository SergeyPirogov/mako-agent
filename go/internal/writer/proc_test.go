package writer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// fakeClaude makes Claude a script with the given body for the test.
func fakeClaude(t *testing.T, body string) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Claude
	t.Cleanup(func() { Claude = old })
	Claude = path
}

// A Claude that hangs with a child holding its output: the timeout ends both, the answer is the timeout's.
func TestAskModelTimeoutKillsChildren(t *testing.T) {
	fakeClaude(t, "sleep 30 & exec sleep 30")
	start := time.Now()
	_, err := AskModel(context.Background(), t.TempDir(), core.Route{Model: "haiku"}, "q", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("error %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

// Mako run from inside Claude Code: the asked Claude is a session of its own.
func TestAskModelCleansEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	fakeClaude(t, `echo "marks:[$CLAUDECODE$CLAUDE_CODE_ENTRYPOINT]"`)
	got, err := AskModel(context.Background(), t.TempDir(), core.Route{Model: "haiku"}, "q", 10*time.Second)
	if err != nil || got != "marks:[]" {
		t.Fatalf("%q %v", got, err)
	}
}

// The reviewer, too: a session of its own, marked as Mako's reviewer.
func TestAgentReviewEnv(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644)
	git("add", "a.go")
	git("commit", "-q", "-m", "add a")

	t.Setenv("CLAUDECODE", "1")
	env := filepath.Join(t.TempDir(), "env")
	fakeClaude(t, `echo "[$CLAUDECODE][$MAKO_REVIEWER]" > `+env+`; echo LGTM`)
	if _, err := AgentReview(context.Background(), repo, "main", core.Task{ID: "a#1", Title: "t"}, core.Route{}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(env); strings.TrimSpace(string(got)) != "[][1]" {
		t.Fatalf("env %q", got)
	}
}
