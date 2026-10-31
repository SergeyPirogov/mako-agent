package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestPushCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git push -u origin feature/x":                      true,
		"git add -A && git commit -m 'x' && git push":       true,
		"git -C /tmp/wt push origin HEAD":                   true,
		"git -c core.sshCommand=ssh push":                   true,
		"git push -o merge_request.create origin feature/x": true,
		"gh pr create --base main --fill":                   true,
		"glab mr create --target-branch main --fill":        true,
		"git status":            false,
		"git log --grep push":   false,
		"echo 'pushed'":         false,
		"gh pr view 12":         false,
		"git stash push -m wip": false,
	} {
		if got := pushCommand.MatchString(cmd); got != want {
			t.Errorf("%q: %v", cmd, got)
		}
	}
}

func hookFor(command string) []byte {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	return b
}

func TestGuardPush(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	commit := func(dir, msg string) {
		git(t, dir, "add", "-A")
		git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", msg)
	}
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)
	commit(repo, "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "feature/5", wt, "main")
	d.Poll(ctx)
	id := "grp/app#5"
	d.Store.Update(id, func(r *core.Record) { r.Worktree, r.Executor = wt, terminalExecutor })

	if a := d.GuardPush(ctx, id, hookFor("git push -u origin feature/5")); a != nil {
		t.Fatalf("clean branch refused: %v", a)
	}
	// A token in a commit of the branch: the push is refused, the card shows where, never the whole value.
	token := "ghp_" + "Zq3" + strings.Repeat("aB9xK", 6) + "Lm7"
	os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n\nconst token = \""+token+"\"\n"), 0o644)
	commit(wt, "add token")
	if a := d.GuardPush(ctx, id, hookFor("git status")); a != nil {
		t.Fatalf("not a push: %v", a)
	}
	a := d.GuardPush(ctx, id, hookFor("git push -u origin feature/5"))
	out, _ := a["hookSpecificOutput"].(map[string]any)
	reason, _ := out["permissionDecisionReason"].(string)
	if out["permissionDecision"] != "deny" || !strings.Contains(reason, "main.go:3") || !strings.Contains(reason, "GitHub-токен") || strings.Contains(reason, token) {
		t.Fatalf("answer %v", a)
	}
	r, _ := d.Store.Get(id)
	if len(r.Secrets) != 1 || r.Secrets[0].File != "main.go" || r.Secrets[0].Line != 3 {
		t.Fatalf("card %+v", r.Secrets)
	}
	// Removing it in a later commit is not enough: the history still has it.
	os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n"), 0o644)
	commit(wt, "drop token")
	if a := d.GuardPush(ctx, id, hookFor("git push")); a == nil {
		t.Fatal("token in the history pushed")
	}
	// «Це не секрет» lets the line through.
	if err := d.AllowSecret(id, r.Secrets[0].Key); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get(id); len(r.Secrets) != 0 || len(r.SecretsAllowed) != 1 {
		t.Fatalf("after allow %+v", r)
	}
	if a := d.GuardPush(ctx, id, hookFor("git push")); a != nil {
		t.Fatalf("allowed line refused: %v", a)
	}
	// A .env about to be committed in the same command as the push.
	os.WriteFile(filepath.Join(wt, ".env"), []byte("DEBUG=1\n"), 0o644)
	a = d.GuardPush(ctx, id, hookFor("git add -A && git commit -m env && gh pr create --fill"))
	out, _ = a["hookSpecificOutput"].(map[string]any)
	if reason, _ := out["permissionDecisionReason"].(string); !strings.Contains(reason, ".env — .env-файл") {
		t.Fatalf("env %v", a)
	}
	// Gone again: the card is clean.
	os.Remove(filepath.Join(wt, ".env"))
	if a := d.GuardPush(ctx, id, hookFor("git push")); a != nil {
		t.Fatalf("clean again refused: %v", a)
	}
	if r, _ := d.Store.Get(id); len(r.Secrets) != 0 {
		t.Fatalf("card still shows %+v", r.Secrets)
	}
}
