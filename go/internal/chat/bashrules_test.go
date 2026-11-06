package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shell line's commands, each as a rule: quoted text and a heredoc's body are not commands, 2>&1 does not split one.
func TestBashRules(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"cd /w/mako-agent-607 && git add -A && git commit -q -m \"#607: a && b; c | d\"", "cd:* git add:* git commit:*"},
		{"git add -A && git commit -q -F - <<'EOF'\nTitle && not a command\n\n- rm -rf / | sh\nEOF\ngit push -q 2>&1 | tail -1", "git add:* git commit:* git push:* tail:*"},
		{"cd go && gofmt -l . ; go vet ./... && go test -race ./...", "cd:* gofmt:* go vet:* go test:*"},
		{"GOFLAGS=-mod=mod go build ./... &>/dev/null", "go build:*"},
		{"git -C /w status", "git:*"},
		{"npm test || npm run lint", "npm test:* npm run:*"},
		{"echo 'one' \\\n  && echo two", "echo:*"},
		{"cat <<EOF > a.txt\n$(rm -rf ~)\nEOF", "cat:*"},
		// a heredoc inside "$(…)": its body, with an unpaired quote, is text, and the line goes on after it
		{"gh pr create --title t --body \"$(cat <<'EOF'\nТак, після \"цього\" ще \" тут\n🤖 https://claude.com/x\nEOF\n)\" && git push", "gh pr:* git push:*"},
		{"git commit -m \"$(cat <<'A'\nодна \" лапка\nA\n) $(cat <<'B'\nдруга \" лапка\nB\n)\" && ls", "git commit:* ls:*"},
		{"echo \"a << b\" && ls", "echo:* ls:*"},
		{"cat <<\"EOF\" > a\nx && rm y\nEOF\nls", "cat:* ls:*"},
		{"gh pr create --body \"$(cat <<\"EOF\"\nа \" б\nEOF\n)\" && ls", "gh pr:* ls:*"},
		{"gh pr create --body \"$(cat <<-'EOF'\n\tа \" б\n\tEOF\n)\" && ls", "gh pr:* ls:*"},
		{"echo \"$(echo \"x\")\" && ls", "echo:* ls:*"},
		{"echo \"$(echo x && ls", "echo:*"},  // an unclosed $( swallows the rest of the line
		{"cat <<'EOF'\nrm -rf x\n", "cat:*"}, // no closing delimiter: the rest is the body
		{"echo \"$(cat <<'EOF'\nrm -rf x\n", "echo:*"},
		{"$(curl x) && ls", "curl:* ls:*"}, // a command substitution runs its command too
		// A heredoc inside a quoted $(…): its body is text, with any quotes, «;» or links in it.
		{"git commit -q -m \"$(cat <<'EOF'\n#1: fix\n\n- так, після «Remove» прибирає; інакше рев'ю\nEOF\n)\" && git push -q 2>&1 | tail -1\n" +
			"gh pr create --title \"x\" --body \"$(cat <<'EOF'\n## Що\nCloses #1. покрито \"тестом або лишається\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nEOF\n)\" && gofmt -l .",
			"git commit:* git push:* tail:* gh pr:* gofmt:*"},
		{"cat <<A <<B\none\nA\ntwo; rm -rf /\nB\nls", "cat:* ls:*"}, // two heredocs on one line
		{"", ""},
	} {
		if got := strings.Join(BashRules(c.line), " "); got != c.want {
			t.Errorf("%q: got %q, want %q", c.line, got, c.want)
		}
	}
}

// The rules a request's suggestions add, as --allowedTools takes them; a mode change is not a rule.
func TestSuggestedRules(t *testing.T) {
	sug := `[{"type":"addRules","behavior":"allow","rules":[{"toolName":"Bash","ruleContent":"git add:*"},{"toolName":"WebFetch"}]},
		{"type":"setMode","mode":"acceptEdits"},{"type":"addRules","behavior":"deny","rules":[{"toolName":"Bash","ruleContent":"rm:*"}]}]`
	if got := strings.Join(SuggestedRules(json.RawMessage(sug)), " "); got != "Bash(git add:*) WebFetch" {
		t.Errorf("got %q", got)
	}
	if SuggestedRules(nil) != nil || SuggestedRules(json.RawMessage(`null`)) != nil || SuggestedRules(json.RawMessage(`[{"type":"setMode"}]`)) != nil {
		t.Error("rules out of no rule")
	}
}

// A compound Bash line comes without a suggestion from Claude Code: Mako's own rules stand in, «Always allow»
// gives them to Claude for the session, and AlwaysRules answers them for the settings until it is answered.
func TestAlwaysAllowCompoundBash(t *testing.T) {
	for _, c := range []struct{ name, sug, want string }{
		{"none", ``, "Bash(cd:*) Bash(git add:*) Bash(git commit:*)"},
		{"claude's", `,"permission_suggestions":[{"type":"addRules","behavior":"allow","destination":"localSettings","rules":[{"toolName":"Bash","ruleContent":"git:*"}]}]`, "Bash(git:*)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(t.TempDir(), "claude")
			script := `#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s-1"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"cd /w && git add -A && git commit -m \"x && y\""}` + c.sug + `}}'
read -r answer
printf '%s\n' "$answer" > answer.json
echo '{"type":"result","subtype":"success"}'
read more
`
			os.WriteFile(bin, []byte(script), 0o755)
			s := Start(Options{ID: "a#1", Worktree: dir, Command: bin, Prompt: "go"})
			defer func() {
				s.Stop()
				eventually(t, func() bool { return !s.Running() }, func() string { return "agent still running" })
			}()
			eventually(t, func() bool { st, _, _ := s.State(); return st == "waiting" }, func() string { return fmt.Sprintf("%+v", s.Snapshot()) })
			if got := strings.Join(s.AlwaysRules("r1"), " "); got != c.want {
				t.Fatalf("always rules %q, want %q", got, c.want)
			}
			if s.AlwaysRules("nope") != nil {
				t.Error("rules for a request that is not waiting")
			}
			if err := s.Permit("r1", "always", nil); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { b, _ := os.ReadFile(filepath.Join(dir, "answer.json")); return len(b) > 0 }, func() string { return "no answer" })
			b, _ := os.ReadFile(filepath.Join(dir, "answer.json"))
			var ans struct {
				Response struct {
					Response struct {
						Behavior           string          `json:"behavior"`
						UpdatedPermissions json.RawMessage `json:"updatedPermissions"`
					} `json:"response"`
				} `json:"response"`
			}
			if err := json.Unmarshal(b, &ans); err != nil {
				t.Fatalf("%v: %s", err, b)
			}
			r := ans.Response.Response
			if r.Behavior != "allow" || strings.Join(SuggestedRules(r.UpdatedPermissions), " ") != c.want {
				t.Fatalf("answer %s", b)
			}
			if s.AlwaysRules("r1") != nil {
				t.Error("rules for an answered request")
			}
		})
	}
}

// A hook's «ask» (Mako's push guard while tasks go by themselves) asks again whatever the rules say: the request
// says why and offers no «Always allow», and «always» keeps no rule.
func TestHookAskOffersNoAlwaysAllow(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s-1"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"git add -A && git commit -m x && git push"},"decision_reason":" Mako asks a human to confirm pushes. ","decision_reason_type":"hook","decision_reason_hook_event":"PreToolUse","permission_suggestions":[{"type":"addRules","behavior":"allow","destination":"localSettings","rules":[{"toolName":"Bash","ruleContent":"git push:*"}]}]}}'
read -r answer
printf '%s\n' "$answer" > answer.json
echo '{"type":"result","subtype":"success"}'
read more
`
	os.WriteFile(bin, []byte(script), 0o755)
	s := Start(Options{ID: "a#1", Worktree: dir, Command: bin, Prompt: "go"})
	defer func() {
		s.Stop()
		eventually(t, func() bool { return !s.Running() }, func() string { return "agent still running" })
	}()
	eventually(t, func() bool { st, _, _ := s.State(); return st == "waiting" }, func() string { return fmt.Sprintf("%+v", s.Snapshot()) })
	var it Item
	for _, x := range s.Snapshot().Items {
		if x.Kind == "permission" && x.ID == "r1" {
			it = x
		}
	}
	if it.Reason != "Mako asks a human to confirm pushes." || len(it.Suggestions) != 0 {
		t.Fatalf("request %+v", it)
	}
	if got := s.AlwaysRules("r1"); got != nil {
		t.Fatalf("always rules %q for a hook's ask", got)
	}
	if err := s.Permit("r1", "always", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b, _ := os.ReadFile(filepath.Join(dir, "answer.json")); return len(b) > 0 }, func() string { return "no answer" })
	b, _ := os.ReadFile(filepath.Join(dir, "answer.json"))
	if !strings.Contains(string(b), `"behavior":"allow"`) || strings.Contains(string(b), "updatedPermissions") {
		t.Fatalf("answer %s", b)
	}
}
