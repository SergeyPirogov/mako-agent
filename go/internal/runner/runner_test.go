package runner

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaude speaks Claude Code's stream-json: it records its arguments and stdin, answers a message with a
// tool permission request, and after the answer finishes the turn with the tool's outcome.
const fakeClaude = `#!/bin/sh
printf '%s\n' "$*" > "$FAKE_DIR/args"
echo '{"type":"system","subtype":"init","session_id":"'"$FAKE_SID"'"}'
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_DIR/stdin"
  case "$line" in
    *'"type":"user"'*)
      echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Запущу тести."},{"type":"tool_use","name":"Bash","input":{"command":"npm test"}}]}}'
      echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"npm test"},"description":"Run tests","permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"behavior":"allow","destination":"localSettings"}]}}' ;;
    *'"behavior":"allow"'*)
      echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Тести пройшли."}]}}'
      echo '{"type":"result","subtype":"success","result":"Тести пройшли.","session_id":"x"}' ;;
    *'"behavior":"deny"'*)
      echo '{"type":"result","subtype":"success","result":"Добре, не запускаю.","session_id":"x"}' ;;
  esac
done
`

func setup(t *testing.T) (*Runner, string, *[]string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaude), 0o755)
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_SID", "sid-1")
	var mu sync.Mutex
	changes := &[]string{}
	r := New(func() Options {
		return Options{Command: filepath.Join(dir, "claude"), PermissionMode: "acceptEdits", AllowedTools: []string{"Bash(git push:*)", "Bash(git commit:*)"}}
	})
	r.OnChange = func(id string) { mu.Lock(); *changes = append(*changes, id); mu.Unlock() }
	return r, dir, changes
}

func wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestSession(t *testing.T) {
	r, dir, _ := setup(t)
	if err := r.Start("t1", t.TempDir(), "sid-1", "Задача: полагодь тести"); err != nil {
		t.Fatal(err)
	}
	wait(t, "the permission request", func() bool { st, _ := r.Status("t1"); return len(st.Requests) == 1 })
	st, _ := r.Status("t1")
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !st.Busy || st.Requests[0].What != "Bash: npm test" || st.Requests[0].Description != "Run tests" {
		t.Fatalf("%+v", st)
	}
	for _, want := range []string{"--input-format stream-json", "--permission-prompt-tool stdio", "--session-id sid-1", "--permission-mode acceptEdits", "--allowedTools Bash(git push:*),Bash(git commit:*)"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("no %q in %s", want, args)
		}
	}
	// Allowed for good: the rule goes back to Claude Code.
	if err := r.Answer("t1", "r1", true, true, "", nil); err != nil {
		t.Fatal(err)
	}
	wait(t, "the end of the turn", func() bool { st, _ := r.Status("t1"); return !st.Busy && st.LastText == "Тести пройшли." })
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.Contains(string(stdin), `"updatedPermissions":[{"type":"addRules"`) || !strings.Contains(string(stdin), `"updatedInput":{"command":"npm test"}`) {
		t.Fatalf("answer sent:\n%s", stdin)
	}
	if err := r.Answer("t1", "r1", true, false, "", nil); err == nil {
		t.Fatal("a request answered twice")
	}

	// After Mako restarts, a message resumes the same session.
	r.Stop("t1")
	wait(t, "the stop", func() bool { st, _ := r.Status("t1"); return !st.Running })
	if st, _ := r.Status("t1"); st.Error != "" {
		t.Fatalf("a stop is not an error: %q", st.Error)
	}
	if err := r.Send("t1", t.TempDir(), "sid-1", "Не запускай тести"); err != nil {
		t.Fatal(err)
	}
	wait(t, "the resume", func() bool {
		args, _ := os.ReadFile(filepath.Join(dir, "args"))
		return strings.Contains(string(args), "--resume sid-1")
	})
	wait(t, "the request", func() bool { st, _ := r.Status("t1"); return len(st.Requests) == 1 })
	r.Answer("t1", "r1", false, false, "Не треба", nil)
	wait(t, "the end", func() bool {
		st, _ := r.Status("t1")
		return !st.Busy && st.LastText == "Добре, не запускаю."
	})
	r.Stop("t1")
}

func TestQuestion(t *testing.T) {
	req := parseRequest("q1", []byte(`{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Який фікс?","header":"Фікс","multiSelect":false,"options":[{"label":"declined"},{"label":"error"}]}]}}`))
	if req == nil || len(req.Questions) != 1 || req.Questions[0].Options[1] != "error" {
		t.Fatalf("%+v", req)
	}
}

func TestMissingClaude(t *testing.T) {
	r := New(func() Options { return Options{Command: "mako-no-such-claude"} })
	if err := r.Start("t", t.TempDir(), NewSessionID(), "x"); err == nil || !strings.Contains(err.Error(), "Claude Code не знайдено") {
		t.Fatalf("%v", err)
	}
}
