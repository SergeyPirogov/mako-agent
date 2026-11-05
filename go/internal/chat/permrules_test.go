package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A rule Mako may keep names its tool, and a Bash rule names a command: «Bash(:*)» would let any line through,
// «Bash(.:*)» any script, and the words of a commit message are no commands.
func TestValidRule(t *testing.T) {
	for rule, want := range map[string]bool{
		"Bash(git add:*)": true, "Bash(go test:*)": true, "Bash(npm run build)": true, "WebFetch": true,
		"mcp__github__get_issue": true, "Read(./src/**)": true, "Bash(./run.sh:*)": true,
		"Bash(:*)": false, "Bash(.:*)": false, "Bash(-:*)": false, "Bash(##:*)": false, "Bash(,:*)": false,
		"Bash()": false, "Bash": true, "": false, "Bash(git add:*": false, "Ba sh(ls:*)": false, "(ls)": false,
		"Bash(ls\nrm:*)": false, "Bash(так:*)": true, // a word is a word: the journal shows it, you decide
	} {
		if got := ValidRule(rule); got != want {
			t.Errorf("%q: %v, want %v", rule, got, want)
		}
	}
}

// Mako's own rules for a compound line skip «commands» that are no commands at all.
func TestBashRulesSkipPunctuation(t *testing.T) {
	if got := strings.Join(BashRules(". ./env.sh && - x; ## y && go test ./..."), " "); got != "go test:*" {
		t.Errorf("got %q", got)
	}
}

// «Always allow selected»: Claude gets only the rules you ticked (and edited) for this agent, never one Mako may
// not keep; the agent's request is noted for the journal with the rules it suggested.
func TestPermitChosenRules(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s-1"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"cd /w && git add -A && go vet ./..."}}}'
read -r answer
printf '%s\n' "$answer" > answer.json
echo '{"type":"result","subtype":"success"}'
read more
`
	os.WriteFile(bin, []byte(script), 0o755)
	var mu sync.Mutex
	var noted []string
	s := Start(Options{ID: "a#1", Worktree: dir, Command: bin, Prompt: "go", OnPermission: func(tool string, input json.RawMessage, rules []string) {
		mu.Lock()
		defer mu.Unlock()
		noted = append(noted, tool+" "+strings.Join(rules, ","))
	}})
	defer func() {
		s.Stop()
		eventually(t, func() bool { return !s.Running() }, func() string { return "agent still running" })
	}()
	eventually(t, func() bool { st, _, _ := s.State(); return st == "waiting" }, func() string { return fmt.Sprintf("%+v", s.Snapshot()) })
	mu.Lock()
	got := strings.Join(noted, "|")
	mu.Unlock()
	if got != "Bash Bash(cd:*),Bash(git add:*),Bash(go vet:*)" {
		t.Fatalf("noted %q", got)
	}
	if err := s.Permit("r1", "always", []string{"Bash(go vet:*)", "Bash(:*)", "Bash(git add -A)"}); err != nil {
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
	if r.Behavior != "allow" || strings.Join(SuggestedRules(r.UpdatedPermissions), " ") != "Bash(go vet:*) Bash(git add -A)" {
		t.Fatalf("answer %s", b)
	}
}
