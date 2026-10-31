package orca

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestTerminals(t *testing.T) {
	cmd, calls := fakeOrca(t, `true`)
	// Answers as Orca's --json does: {ok, result, _meta}.
	os.WriteFile(cmd, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+calls+`
case "$1 $2" in
  "terminal list") echo '{"id":"1","ok":true,"result":{"terminals":[{"handle":"term_1","title":"zsh","connected":true,"lastOutputAt":1700000000000,"preview":"$"},{"handle":"term_2","title":"claude","agentIdentity":"claude","connected":true,"lastOutputAt":1700000005000,"preview":"Running tests"}],"totalCount":2,"truncated":false},"_meta":{"runtimeId":"r"}}' ;;
  "terminal show") echo '{"ok":true,"result":{"terminal":{"handle":"term_2","agentWait":{"source":"hook","reason":"permission","since":1700000006000}}}}' ;;
  "terminal read") echo '{"ok":true,"result":{"terminal":{"handle":"term_2","status":"running","tail":["Do you want to proceed?","❯ 1. Yes",""],"truncated":false,"nextCursor":null,"source":"screen"}}}' ;;
  *) echo '{"ok":false,"error":{"code":"bad","message":"unknown"}}'; exit 1 ;;
esac
`), 0o755)
	o := config.Orca{Command: cmd}
	ctx := context.Background()
	wt := t.TempDir()
	ts, _, err := Terminals(ctx, o, wt, "")
	if err != nil || len(ts) != 2 {
		t.Fatalf("%v %+v", err, ts)
	}
	agent := AgentTerminal(ts)
	if agent.Handle != "term_2" || agent.Agent != "claude" || agent.LastOutputAt != 1700000005000 {
		t.Fatalf("agent terminal %+v", agent)
	}
	w, err := Waiting(ctx, o, wt, agent.Handle)
	if err != nil || w == nil || w.Reason != "permission" {
		t.Fatalf("wait %v %+v", err, w)
	}
	screen, err := Screen(ctx, o, wt, agent.Handle)
	if err != nil || screen[0] != "Do you want to proceed?" {
		t.Fatalf("screen %v %q", err, screen)
	}
	log, _ := os.ReadFile(calls)
	for _, want := range []string{"terminal list --worktree path:" + wt + " --json", "terminal show --terminal term_2 --json", "terminal read --terminal term_2 --screen --json"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
	if err := Focus(ctx, o, wt, "term_2"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("a failing command should say why: %v", err)
	}
}

func TestWorkspaces(t *testing.T) {
	cmd, calls := fakeOrca(t, `true`)
	os.WriteFile(cmd, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+calls+`
echo '{"id":"1","ok":true,"result":{"worktrees":[{"worktreeId":"w1","path":"/Users/s/orca/market-autotests-31","branch":"refs/heads/fix/31-rozibraty","status":"working","sidebarStatus":"working","liveTerminalCount":0,"lastOutputAt":null,"preview":"","linkedPR":null,"agents":[{"paneKey":"p1","parentPaneKey":null,"state":"working","agentType":"claude","prompt":"You are working…","taskTitle":null,"displayName":null,"lastAssistantMessage":"Now look at the screenshots.","toolName":"Read","toolInput":"att/a0243a71.jpg","interrupted":false,"agentWait":null,"stateStartedAt":1700000000000,"updatedAt":1700000060000}]}],"totalCount":1,"truncated":false},"_meta":{"runtimeId":"r"}}'
`), 0o755)
	ws, err := Workspaces(context.Background(), config.Orca{Command: cmd})
	if err != nil || len(ws) != 1 {
		t.Fatalf("%v %+v", err, ws)
	}
	w := FindWorkspace(ws, "/nowhere", "fix/31-rozibraty")
	if w == nil || w.Status != "working" {
		t.Fatalf("by branch: %+v", w)
	}
	a := w.MainAgent()
	if a == nil || a.AgentType != "claude" || a.ToolName != "Read" || a.LastAssistantMessage != "Now look at the screenshots." || a.AgentWait != nil {
		t.Fatalf("agent %+v", a)
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), "worktree ps --agent-wait --json") {
		t.Fatalf("calls %s", log)
	}
}

func TestLastMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	wt := "/Users/s/orca/workspaces/market-autotests/market-autotests-31"
	dir := filepath.Join(home, ".claude", "projects", "-Users-s-orca-workspaces-market-autotests-market-autotests-31")
	os.MkdirAll(dir, 0o755)
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"Розбери нічний ран"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Спершу подивлюсь звіт."}]}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","name":"Read","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"…"}]}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"Ось коротша версія:\n\n| # | Тести |\n|---|---|\n| 1 | @67276 |"}]}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"Я досі нічого не змінював."}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"id":"s1","role":"assistant","content":[{"type":"text","text":"subagent noise"}]}}`,
	}
	os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte(`{"type":"assistant","message":{"id":"x","role":"assistant","content":[{"type":"text","text":"old session"}]}}`+"\n"), 0o644)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "new.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	got := LastMessage(wt)
	want := "Ось коротша версія:\n\n| # | Тести |\n|---|---|\n| 1 | @67276 |\n\nЯ досі нічого не змінював."
	if got != want {
		t.Fatalf("got %q", got)
	}
	if LastMessage("/no/such/worktree") != "" {
		t.Fatal("a message for a worktree without sessions")
	}
}

func TestConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	wt := "/Users/s/orca/workspaces/app/app-7"
	dir := filepath.Join(home, ".claude", "projects", "-Users-s-orca-workspaces-app-app-7")
	os.MkdirAll(dir, 0o755)
	lines := []string{
		`{"type":"user","timestamp":"2026-10-31T08:15:00Z","message":{"role":"user","content":"You are working on GitLab issue app#7."}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>…"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Подивлюсь звіт."},{"type":"tool_use","name":"Read","input":{"file_path":"/w/summary.md"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"big file"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"npm   test\n -- payment","description":"Run"}}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"Так, запускай"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Готово: 3 тести впали."}]}}`,
	}
	os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	got := Conversation(wt, 100)
	want := []string{
		"user You are working on GitLab issue app#7.",
		"agent Подивлюсь звіт.",
		"tool Read: /w/summary.md",
		"tool Bash: npm test -- payment",
		"user Так, запускай",
		"agent Готово: 3 тести впали.",
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].Role+" "+got[i].Text != w {
			t.Errorf("%d: %q, want %q", i, got[i].Role+" "+got[i].Text, w)
		}
	}
	if last := Conversation(wt, 2); len(last) != 2 || last[1].Text != "Готово: 3 тести впали." {
		t.Fatalf("limit: %+v", last)
	}
}
