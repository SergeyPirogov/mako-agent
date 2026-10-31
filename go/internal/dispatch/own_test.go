package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/runner"
)

// A fake Claude Code for Mako's own agent: asks to run the tests, and after the answer finishes its turn.
const ownClaude = `#!/bin/sh
printf '%s\n' "$*" > "$OWN_DIR/args"
pwd > "$OWN_DIR/cwd"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$OWN_DIR/stdin"
  case "$line" in
    *'"type":"user"'*)
      echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"npm test"}}]}}'
      echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"npm test"}}}' ;;
    *'"behavior"'*)
      echo '{"type":"result","subtype":"success","result":"Готово: MR відкрито."}' ;;
  esac
done
`

func TestOwnAgent(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	d.Cfg.Agent.Runner = "mako"

	// The repo's origin is GitLab's address; git reaches it in a local bare repo.
	repo := d.Cfg.Trackers[0].Projects["grp/app"].Repos["app"]
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare)
	os.WriteFile(filepath.Join(repo.Path, "README"), []byte("app\n"), 0o644)
	git(t, repo.Path, "add", ".")
	git(t, repo.Path, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	origin := strings.TrimSpace(gitOut(t, repo.Path, "config", "--get", "remote.origin.url"))
	git(t, repo.Path, "config", "url."+bare+".insteadOf", origin)
	git(t, repo.Path, "push", "-q", "origin", "main")

	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "claude"), []byte(ownClaude), 0o755)
	t.Setenv("OWN_DIR", fake)
	d.Runner = runner.New(func() runner.Options {
		return runner.Options{Command: filepath.Join(fake, "claude"), PermissionMode: "acceptEdits"}
	})
	d.Runner.OnChange = d.AgentChanged

	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.HandedOff || r.Executor != "mako" || r.SessionID == "" || len(*jobs) != 0 {
		t.Fatalf("not started by Mako: %+v (orca jobs %d)", r, len(*jobs))
	}
	// Its own worktree, on the task's branch, from origin/main.
	if b := strings.TrimSpace(gitOut(t, r.Worktree, "rev-parse", "--abbrev-ref", "HEAD")); b != r.Branch {
		t.Fatalf("worktree on %q, want %q", b, r.Branch)
	}
	waitFor(t, "the agent's request", func() bool {
		d.WatchAgents(ctx, time.Now())
		a := d.Agents()["grp/app#5"]
		return a.State == "waiting" && len(a.Requests) == 1
	})
	a := d.Agents()["grp/app#5"]
	if !a.Own || a.Reason != "дозвіл на Bash: npm test" {
		t.Fatalf("agent %+v", a)
	}
	cwd, _ := os.ReadFile(filepath.Join(fake, "cwd"))
	stdin, _ := os.ReadFile(filepath.Join(fake, "stdin"))
	if strings.TrimSpace(string(cwd)) != r.Worktree && !strings.HasSuffix(strings.TrimSpace(string(cwd)), filepath.Base(r.Worktree)) {
		t.Fatalf("agent ran in %s, not %s", cwd, r.Worktree)
	}
	if !strings.Contains(string(stdin), "Кнопка оплати не працює") || !strings.Contains(string(stdin), "git push -u origin "+r.Branch) {
		t.Fatalf("the prompt:\n%s", stdin)
	}

	// Allowed from the board: the turn ends.
	if err := d.AnswerAgent("grp/app#5", "r1", true, false, "", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the end of the turn", func() bool {
		d.WatchAgents(ctx, time.Now())
		return d.Agents()["grp/app#5"].State == "done"
	})
	if a := d.Agents()["grp/app#5"]; a.Message != "Готово: MR відкрито." {
		t.Fatalf("done: %+v", a)
	}

	// A message to the agent.
	if err := d.SendToAgent("grp/app#5", "Ще додай тест"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the message", func() bool {
		b, _ := os.ReadFile(filepath.Join(fake, "stdin"))
		return strings.Contains(string(b), "Ще додай тест")
	})

	// MR, then merged: the agent stops and its worktree goes.
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 9, "state": "merged", "web_url": f.base + "/grp/app/-/merge_requests/9",
		"source_branch": r.Branch, "title": "Fix the button", "description": strings.Repeat("described ", 20)})
	f.mu.Unlock()
	d.CheckHandoffs(ctx)
	d.CheckMerges(ctx)
	if _, err := os.Stat(r.Worktree); err == nil {
		t.Fatal("the worktree stayed after the merge")
	}
	waitFor(t, "the agent stopped", func() bool { st, _ := d.Runner.Status("grp/app#5"); return !st.Running })
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
