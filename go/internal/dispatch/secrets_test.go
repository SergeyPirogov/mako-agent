package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
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
	if out["permissionDecision"] != "deny" || !strings.Contains(reason, "main.go:3") || !strings.Contains(reason, "GitHub token") || strings.Contains(reason, token) {
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
	if reason, _ := out["permissionDecisionReason"].(string); !strings.Contains(reason, ".env — .env file") {
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
	// Unattended (AutoTake, AutoFix): a clean push or a new MR waits for you; one with secrets is still refused.
	decision := func(a map[string]any) string {
		out, _ := a["hookSpecificOutput"].(map[string]any)
		s, _ := out["permissionDecision"].(string)
		return s
	}
	for _, turn := range []func(*config.Config){
		func(c *config.Config) { c.Notifications.AutoTake = true },
		func(c *config.Config) { c.Terminal.AutoFix = true },
	} {
		cfg := *d.Config()
		turn(&cfg)
		d.Cfg = &cfg
		for _, cmd := range []string{"git push -u origin feature/5", "gh pr create --fill", "glab mr create --fill"} {
			if a := d.GuardPush(ctx, id, hookFor(cmd)); decision(a) != "ask" {
				t.Fatalf("%q unattended: %v", cmd, a)
			}
		}
		if a := d.GuardPush(ctx, id, hookFor("go test ./...")); a != nil {
			t.Fatalf("not a push, unattended: %v", a)
		}
		os.WriteFile(filepath.Join(wt, ".env"), []byte("DEBUG=1\n"), 0o644)
		if a := d.GuardPush(ctx, id, hookFor("git push")); decision(a) != "deny" {
			t.Fatalf("secrets unattended: %v", a)
		}
		os.Remove(filepath.Join(wt, ".env"))
		cfg.Notifications.AutoTake, cfg.Terminal.AutoFix = false, false
	}
}

// guardRepo: the task grp/app#5 on a worktree of the clone, with a bare origin to push to.
func guardRepo(t *testing.T) (d *Dispatcher, id, wt string) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	d, _, _ = setup(t)
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(origin), "init", "-q", "--bare", "-b", "main", origin)
	git(t, repo, "remote", "set-url", "origin", origin)
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
	git(t, repo, "push", "-q", "origin", "main")
	wt = filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "feature/5", wt, "main")
	d.Poll(context.Background())
	id = "grp/app#5"
	d.Store.Update(id, func(r *core.Record) { r.Worktree, r.Executor = wt, terminalExecutor })
	return d, id, wt
}

// A check that fails does not let the push through: the agent hears why.
func TestGuardPushStopsWhenCheckFails(t *testing.T) {
	d, id, _ := guardRepo(t)
	old := pushDiff
	t.Cleanup(func() { pushDiff = old })
	pushDiff = func(context.Context, string, string) (string, error) { return "", errors.New("git log: broken") }
	a := d.GuardPush(context.Background(), id, hookFor("git push -u origin feature/5"))
	out, _ := a["hookSpecificOutput"].(map[string]any)
	reason, _ := out["permissionDecisionReason"].(string)
	if out["permissionDecision"] != "deny" || !strings.Contains(reason, "could not check") || !strings.Contains(reason, "git log: broken") {
		t.Fatalf("answer %v", a)
	}
}

// The worktree's pre-push hook: any push from the task's worktree asks Mako, which stops one carrying secrets,
// one it cannot check, and one with a token it does not know.
func TestPrePushHook(t *testing.T) {
	d, id, wt := guardRepo(t)
	ctx := context.Background()
	pushHooks = true
	t.Cleanup(func() { pushHooks = false })
	d.Sessions = term.NewManager(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, reason := d.PrePush(r.Context(), r.URL.Query().Get("id"), r.URL.Query().Get("token"))
		w.WriteHeader(code)
		io.WriteString(w, reason)
	}))
	defer srv.Close()
	d.BoardURL = srv.URL

	if code, _ := d.PrePush(ctx, id, ""); code != http.StatusForbidden {
		t.Fatalf("no hook yet: %d", code)
	}
	if err := d.installPushHook(ctx, id, wt); err != nil {
		t.Fatal(err)
	}
	token := d.pushToken(id)
	if err := d.installPushHook(ctx, id, wt); err != nil || d.pushToken(id) != token || token == "" {
		t.Fatalf("the token changed at the next start: %v", err)
	}
	if code, _ := d.PrePush(ctx, id, "wrong"); code != http.StatusForbidden {
		t.Fatalf("a wrong token: %d", code)
	}
	push := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Clean: pushed.
	if out, err := push("push", "-q", "origin", "HEAD:refs/heads/feature/5"); err != nil {
		t.Fatalf("clean push stopped: %v %s", err, out)
	}
	// A token, pushed by a script the command check cannot read: stopped, the card shows where.
	tok := "ghp_" + "Zq3" + strings.Repeat("aB9xK", 6) + "Lm7"
	os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n\nconst token = \""+tok+"\"\n"), 0o644)
	git(t, wt, "commit", "-q", "-am", "add token")
	os.WriteFile(filepath.Join(wt, "ship.sh"), []byte("#!/bin/sh\ngit push origin HEAD:refs/heads/feature/5\n"), 0o755)
	out, err := push("-c", "alias.ship=!sh ship.sh", "ship")
	if err == nil || !strings.Contains(out, "main.go:3") || !strings.Contains(out, "GitHub token") || strings.Contains(out, tok) {
		t.Fatalf("token pushed: %v %s", err, out)
	}
	if r, _ := d.Store.Get(id); len(r.Secrets) != 1 {
		t.Fatalf("card %+v", r.Secrets)
	}
	// The check fails: stopped too.
	old := pushDiff
	t.Cleanup(func() { pushDiff = old })
	pushDiff = func(context.Context, string, string) (string, error) { return "", errors.New("git log: broken") }
	if code, reason := d.PrePush(ctx, id, token); code != http.StatusConflict || !strings.Contains(reason, "could not check") {
		t.Fatalf("failed check: %d %s", code, reason)
	}
}
