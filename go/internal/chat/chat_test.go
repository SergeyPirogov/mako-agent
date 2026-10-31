package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stand-in for claude: the same JSON lines, one turn with a permission, then it waits for more.
const fakeClaude = `#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s-1","model":"claude-opus-5-5"}'
echo '{"type":"assistant","message":{"usage":{"input_tokens":2,"cache_read_input_tokens":90000,"cache_creation_input_tokens":10000},"content":[{"type":"text","text":"Пишу файл"},{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"a.txt","content":"hi"}}]},"parent_tool_use_id":null}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"a.txt","content":"hi"},"permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}'
read answer
echo "$answer" > answer.json
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"written","is_error":false}]},"parent_tool_use_id":null}'
echo '{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.3,"resetsAt":100},"seven_day":{"utilization":0.07,"resetsAt":200}}}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"DONE"}]},"parent_tool_use_id":null}'
echo '{"type":"result","subtype":"success","duration_ms":1500,"total_cost_usd":0.01,"modelUsage":{"claude-opus-5-5":{"contextWindow":1000000}}}'
read more
echo "$more" > more.json
`

func TestChat(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaude), 0o755)
	var limits Limits
	var session string
	var cost float64
	s := Start(Options{ID: "a#1", Worktree: dir, Command: bin, Setup: "echo prepared", Prompt: "Зроби задачу",
		OnLimits: func(l Limits) { limits = l }, OnSession: func(id string) { session = id }, OnCost: func(usd float64) { cost += usd }})
	defer s.Stop()
	wait := func(state string) {
		t.Helper()
		for i := 0; i < 300; i++ {
			if st, _, _ := s.State(); st == state {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("no %s: %+v", state, s.Snapshot())
	}
	wait("waiting")
	if _, tool, _ := s.State(); tool != "Write: a.txt" {
		t.Fatalf("waiting for %q", tool)
	}
	if err := s.Permit("r1", "always"); err != nil {
		t.Fatal(err)
	}
	wait("done")
	if b, _ := os.ReadFile(filepath.Join(dir, "answer.json")); !strings.Contains(string(b), `"behavior":"allow"`) || !strings.Contains(string(b), `"updatedPermissions"`) {
		t.Fatalf("answer %s", b)
	}
	snap := s.Snapshot()
	kinds := []string{}
	for _, it := range snap.Items {
		kinds = append(kinds, it.Kind+":"+it.Status)
	}
	if strings.Join(kinds, " ") != "user: tool:ok text: tool:ok permission:always text: result:" {
		t.Fatalf("items %v", kinds)
	}
	if snap.Items[1].Output != "prepared\n" || snap.Items[3].Output != "written" || s.LastText() != "DONE" {
		t.Fatalf("outputs %+v", snap.Items)
	}
	if cost != 0.01 {
		t.Fatalf("cost %v", cost)
	}
	if session != "s-1" || limits.FiveHour == nil || limits.FiveHour.Used != 0.3 || limits.Week.ResetsAt != 200 {
		t.Fatalf("session %q limits %+v", session, limits)
	}
	if snap.Model != "claude-opus-5-5" || snap.Context != 100002 || snap.Window != 1000000 || snap.Mode != "default" {
		t.Fatalf("model %q context %d/%d mode %q", snap.Model, snap.Context, snap.Window, snap.Mode)
	}
	if err := s.Send("Далі"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300 && s.Running(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "more.json")); !strings.Contains(string(b), "Далі") {
		t.Fatalf("more %s", b)
	}
	if s.Running() || s.Snapshot().State != "exited" {
		t.Fatal("still running")
	}
}

func TestHistory(t *testing.T) {
	dir := t.TempDir()
	history := filepath.Join(dir, "session.jsonl")
	os.WriteFile(history, []byte(strings.Join([]string{
		`{"type":"summary","summary":"Night run"}`,
		`{"type":"user","message":{"role":"user","content":"Розбери нічний ран"},"parent_tool_use_id":null}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"Caveat: local commands"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Дивлюсь пайплайн"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"glab ci view"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"12 failed"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"sleep 100"}}]}}`,
	}, "\n")), 0o644)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\nread init\nread more\n"), 0o755)
	s := Start(Options{ID: "a#1", Worktree: dir, Command: bin, History: history})
	defer s.Stop()
	for i := 0; i < 300; i++ {
		if st, _, _ := s.State(); st == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := []string{}
	for _, it := range s.Snapshot().Items {
		got = append(got, it.Kind+":"+it.Status+":"+it.Text+it.Output)
	}
	if strings.Join(got, " | ") != "user::Розбери нічний ран | text::Дивлюсь пайплайн | tool:ok:12 failed | tool:error:" {
		t.Fatalf("history %v", got)
	}
}

// The agent asks (AskUserQuestion), you answer, its turn ends; left waiting for you, it is closed, asleep.
const askClaude = `#!/bin/sh
read init
read msg
echo '{"type":"control_request","request_id":"q1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Яку гілку?","header":"Гілка","multiSelect":false,"options":[{"label":"main"},{"label":"master"}]}]}}}'
read answer
echo "$answer" > answer.json
echo '{"type":"result","subtype":"success","duration_ms":10}'
read more
`

func TestAskAndSleep(t *testing.T) {
	old := IdleAfter
	IdleAfter = 200 * time.Millisecond
	defer func() { IdleAfter = old }()
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(askClaude), 0o755)
	s := Start(Options{ID: "a#2", Worktree: dir, Command: bin, Prompt: "Почни"})
	defer s.Stop()
	until := func(ok func() bool) {
		t.Helper()
		for i := 0; i < 300 && !ok(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if !ok() {
			t.Fatalf("not yet: %+v", s.Snapshot())
		}
	}
	until(func() bool { st, _, _ := s.State(); return st == "waiting" })
	if err := s.Answer("q1", map[string]string{"Яку гілку?": "master"}); err != nil {
		t.Fatal(err)
	}
	until(func() bool { st, _, _ := s.State(); return st == "done" })
	if b, _ := os.ReadFile(filepath.Join(dir, "answer.json")); !strings.Contains(string(b), `"answers":{"Яку гілку?":"master"}`) || !strings.Contains(string(b), `"questions"`) {
		t.Fatalf("answer %s", b)
	}
	if it := s.Snapshot().Items[1]; it.Status != "answered" || it.Answers["Яку гілку?"] != "master" {
		t.Fatalf("item %+v", it)
	}
	until(func() bool { return !s.Running() })
	if snap := s.Snapshot(); !snap.Asleep || snap.Live {
		t.Fatalf("not asleep: %+v", snap)
	}
	for _, it := range s.Snapshot().Items {
		if it.Kind == "error" {
			t.Fatalf("closing for idleness is no error: %+v", it)
		}
	}
}

// A message that resumes a conversation shows after the history it goes on from.
func TestResumeWithMessage(t *testing.T) {
	dir := t.TempDir()
	history := filepath.Join(dir, "session.jsonl")
	os.WriteFile(history, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}`+"\n"), 0o644)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\nread init\nread msg\necho \"$msg\" > "+filepath.Join(dir, "msg.json")+"\nread more\n"), 0o755)
	s := Start(Options{ID: "a#3", Worktree: dir, Command: bin, History: history, Prompt: "Ще тести"})
	defer s.Stop()
	for i := 0; i < 300; i++ {
		if b, _ := os.ReadFile(filepath.Join(dir, "msg.json")); strings.Contains(string(b), "Ще тести") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := []string{}
	for _, it := range s.Snapshot().Items {
		got = append(got, it.Kind+":"+it.Text)
	}
	if strings.Join(got, " | ") != "text:Готово | user:Ще тести" {
		t.Fatalf("items %v", got)
	}
}
