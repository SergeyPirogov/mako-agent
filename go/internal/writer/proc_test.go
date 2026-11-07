package writer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if err == nil || !strings.Contains(err.Error(), "no answer from Claude") {
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

// A failed Claude's error says why: its stderr, else what it printed to stdout, else its result line.
func TestAskModelFailureSaysWhy(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"stderr": {`echo "boom: bad flag" >&2; exit 1`, "boom: bad flag"},
		"stdout": {`echo "Usage limit reached"; exit 1`, "Usage limit reached"},
		"result": {`echo '{"type":"result","is_error":true,"result":"You hit your limit"}'; exit 1`, "You hit your limit"},
		"silent": {`exit 1`, "no output"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeClaude(t, tc.body)
			_, err := AskModel(context.Background(), t.TempDir(), core.Route{Model: "haiku"}, "q", 10*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestFailDetailKeepsTail(t *testing.T) {
	got := failDetail(strings.Repeat("я", 1000)+"END", nil)
	if !strings.HasSuffix(got, "END") || len(got) > failDetailMax+4 || !utf8.ValidString(got) {
		t.Fatalf("bad tail: %q", got)
	}
}

// A failed Claude is told apart by what it said: the account's limit and the login are not a fault of Mako.
func TestClaudeErrorKinds(t *testing.T) {
	for name, tc := range map[string]struct {
		body         string
		limit, login bool
	}{
		"limit":  {`echo '{"type":"result","is_error":true,"result":"You'"'"'ve hit your session limit · resets 3pm"}'; exit 1`, true, false},
		"login":  {`echo "Invalid API key · Please run /login" >&2; exit 1`, false, true},
		"other":  {`echo "boom" >&2; exit 1`, false, false},
		"silent": {`exit 1`, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			fakeClaude(t, tc.body)
			_, err := AskModel(context.Background(), t.TempDir(), core.Route{Model: "haiku"}, "q", 10*time.Second)
			var ce *ClaudeError
			if !errors.As(err, &ce) {
				t.Fatalf("error %T %v, want a ClaudeError", err, err)
			}
			if LimitReached(err) != tc.limit || LoginFailed(err) != tc.login {
				t.Fatalf("limit=%v login=%v for %v", LimitReached(err), LoginFailed(err), err)
			}
		})
	}
	if LimitReached(errors.New("claude: exit status 1: hit your limit")) || LoginFailed(nil) {
		t.Fatal("only a ClaudeError is told by its words")
	}
}

// The wordings of Claude Code known now. A new one that is not told here is kept among Mako's errors again: add it.
func TestClaudeErrorWordings(t *testing.T) {
	err := func(detail string) error { return &ClaudeError{Err: errors.New("exit status 1"), Detail: detail} }
	for _, w := range []string{"You've hit your session limit · resets 3pm", "You've hit your weekly limit", "Claude AI usage limit reached", "Usage limit reached"} {
		if !LimitReached(err(w)) || LoginFailed(err(w)) {
			t.Errorf("%q is a limit", w)
		}
	}
	for _, w := range []string{"Failed to authenticate", "Invalid API key · Please run /login", "Run claude auth login", "Please run /login"} {
		if !LoginFailed(err(w)) || LimitReached(err(w)) {
			t.Errorf("%q is a login failure", w)
		}
	}
	if LimitReached(err("boom")) || LoginFailed(err("boom")) || !err(noOutputDetail).(*ClaudeError).Silent() {
		t.Error("an ordinary failure is neither; only the one with no output is silent")
	}
}
