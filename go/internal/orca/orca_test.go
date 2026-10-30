package orca

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// fakeOrca writes a script that records its arguments and answers like `orca worktree create --json`.
func fakeOrca(t *testing.T, create string) (string, string) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + calls + `
case "$1 $2" in
  "worktree create") ` + create + ` ;;
  *) echo '{"ok":true}' ;;
esac
`
	path := filepath.Join(dir, "orca")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func job(t *testing.T) Job {
	return Job{
		Task:        core.Task{ID: "grp/app#5", Slug: "app-5", Title: "Fix pay", URL: "https://gl/grp/app/-/issues/5", ClosingRef: "Closes grp/app#5"},
		TrackerName: "GitLab",
		Repo:        &config.Repo{Path: t.TempDir(), BaseBranch: "main"},
		Branch:      "fix/5-fix-pay",
	}
}

func TestHand(t *testing.T) {
	cmd, calls := fakeOrca(t, `echo '{"ok":true,"result":{"worktree":{"path":"/wt/app-5","branch":"refs/heads/fix/5-fix-pay-1"}}}'`)
	res, err := Hand(context.Background(), config.Orca{Command: cmd, LinkIssue: true}, job(t), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Worktree != "/wt/app-5" || res.Branch != "fix/5-fix-pay-1" {
		t.Fatalf("result %+v", res)
	}
	log, _ := os.ReadFile(calls)
	for _, want := range []string{"--branch fix/5-fix-pay", "--base-branch main", "--agent claude", "--reference https://gl/grp/app/-/issues/5",
		"git push -u origin fix/5-fix-pay", "Closes grp/app#5", "--workspace-status in-progress"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
}

func TestHandRetriesWithoutLink(t *testing.T) {
	cmd, calls := fakeOrca(t, `case "$*" in *--reference*) echo '{"ok":false,"error":{"code":"bad_input","message":"unknown issue source for reference"}}'; exit 1;; esac
echo '{"ok":true,"worktree":{"path":"/wt/x","branch":"fix/5-fix-pay"}}'`)
	res, err := Hand(context.Background(), config.Orca{Command: cmd, LinkIssue: true}, job(t), func(string) {})
	if err != nil || res.Worktree != "/wt/x" {
		t.Fatalf("%v %+v", err, res)
	}
	log, _ := os.ReadFile(calls)
	if strings.Count(string(log), "worktree create") != 2 {
		t.Fatalf("calls:\n%s", log)
	}
}

func TestHandExplainsFailure(t *testing.T) {
	cmd, _ := fakeOrca(t, `echo '{"ok":false,"error":{"code":"repo_not_found","message":"Repo is not added","data":{"nextSteps":["Add the repo in Orca"]}}}'; exit 1`)
	_, err := Hand(context.Background(), config.Orca{Command: cmd, LinkIssue: true}, job(t), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "repo_not_found") || !strings.Contains(err.Error(), "Orca радить: Add the repo in Orca") {
		t.Fatalf("error: %v", err)
	}
}

func TestInstructions(t *testing.T) {
	j := job(t)
	me := Instructions("", "me", j)
	if !strings.Contains(me, "Do not push") || strings.Contains(me, "{branch}") {
		t.Fatalf("for me:\n%s", me)
	}
	if got := Instructions("Візьми {taskId} у {branch} → {baseBranch}, {unknown}", "agent", j); got != "Візьми grp/app#5 у fix/5-fix-pay → main, {unknown}" {
		t.Fatalf("custom: %s", got)
	}
}
