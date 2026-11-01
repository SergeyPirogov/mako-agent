package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eventually waits for ok to hold. The fake agents are shell scripts, slow to start when go test ./...
// runs every package at once, so the deadline is generous: it costs nothing when ok comes true early.
func eventually(t *testing.T, ok func() bool, why func() string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(why())
		}
	}
}

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
		OnLimits: func(l Limits) { limits = l }, OnSession: func(id, _ string) { session = id }, OnCost: func(usd float64) { cost += usd }})
	defer s.Stop()
	wait := func(state string) {
		t.Helper()
		eventually(t, func() bool { st, _, _ := s.State(); return st == state }, func() string { return fmt.Sprintf("no %s: %+v", state, s.Snapshot()) })
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
	eventually(t, func() bool { return !s.Running() }, func() string { return "still running" })
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
	eventually(t, func() bool { st, _, _ := s.State(); return st == "done" }, func() string { return fmt.Sprintf("not done: %+v", s.Snapshot()) })
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
		eventually(t, ok, func() string { return fmt.Sprintf("not yet: %+v", s.Snapshot()) })
	}
	until(func() bool { st, _, _ := s.State(); return st == "waiting" })
	it := s.Asked()
	if it == nil || it.ID != "q1" {
		t.Fatalf("asked %+v", it)
	}
	if q, options, ok := Choices(*it); !ok || q != "Яку гілку?" || strings.Join(options, ",") != "main,master" {
		t.Fatalf("choices %q %v %v", q, options, ok)
	}
	if err := s.Choose("q1", 5); err == nil {
		t.Fatal("no such option, yet chosen")
	}
	// A notification's button: option 1 is «master».
	if err := s.Choose("q1", 1); err != nil {
		t.Fatal(err)
	}
	if s.Asked() != nil || s.Choose("q1", 1) == nil {
		t.Fatal("answered twice")
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
	eventually(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "msg.json"))
		return strings.Contains(string(b), "Ще тести")
	}, func() string { return "no message sent" })
	got := []string{}
	for _, it := range s.Snapshot().Items {
		got = append(got, it.Kind+":"+it.Text)
	}
	if strings.Join(got, " | ") != "text:Готово | user:Ще тести" {
		t.Fatalf("items %v", got)
	}
}

// A stand-in for claude in plan mode: it offers a plan, gets it back with notes, offers another.
const planClaude = `#!/bin/sh
echo "$@" > args.txt
read init
read msg
echo '{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Зробити"}}}'
read answer
echo "$answer" > revise.json
echo '{"type":"control_request","request_id":"p2","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Зробити й протестувати"}}}'
read answer
echo "$answer" > approve.json
read mode
echo "$mode" > mode.json
echo '{"type":"result","subtype":"success","duration_ms":10,"total_cost_usd":0.01}'
read more
`

func TestPlan(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(planClaude), 0o755)
	s := Start(Options{ID: "a#3", Worktree: dir, Command: bin, Prompt: "Зроби", Mode: "plan"})
	defer s.Stop()
	asked := func(id string) {
		t.Helper()
		eventually(t, func() bool { it := s.Asked(); return it != nil && it.ID == id }, func() string { return fmt.Sprintf("no plan %s: %+v", id, s.Snapshot()) })
	}
	read := func(name string) string {
		t.Helper()
		var b []byte
		eventually(t, func() bool { b, _ = os.ReadFile(filepath.Join(dir, name)); return len(b) > 0 }, func() string { return "no " + name })
		return string(b)
	}
	asked("p1")
	if args := read("args.txt"); !strings.Contains(args, "--permission-mode plan") {
		t.Fatalf("args %s", args)
	}
	if s.RevisePlan("p1", "  ") == nil {
		t.Fatal("empty notes sent")
	}
	if err := s.RevisePlan("p1", "Додай тести"); err != nil {
		t.Fatal(err)
	}
	if b := read("revise.json"); !strings.Contains(b, `"behavior":"deny"`) || !strings.Contains(b, "Додай тести") {
		t.Fatalf("revise %s", b)
	}
	asked("p2")
	if err := s.ApprovePlan("p2", "acceptEdits"); err != nil {
		t.Fatal(err)
	}
	if b := read("approve.json"); !strings.Contains(b, `"behavior":"allow"`) || !strings.Contains(b, `"setMode"`) || !strings.Contains(b, `"plan":"1. Зробити й протестувати"`) {
		t.Fatalf("approve %s", b)
	}
	if b := read("mode.json"); !strings.Contains(b, `"set_permission_mode"`) || !strings.Contains(b, `"acceptEdits"`) {
		t.Fatalf("mode %s", b)
	}
	if s.ApprovePlan("p2", "acceptEdits") == nil {
		t.Fatal("approved twice")
	}
	snap := s.Snapshot()
	var got []string
	for _, it := range snap.Items {
		if it.Kind == "permission" {
			got = append(got, it.ID+":"+it.Status+":"+it.Text)
		}
	}
	if strings.Join(got, " ") != "p1:revised:Додай тести p2:approved:" || snap.Mode != "acceptEdits" {
		t.Fatalf("items %v mode %q", got, snap.Mode)
	}
}

// With Tools, a request for any other tool is denied at once and one of them is allowed: nobody is asked.
func TestOnlyTools(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(`#!/bin/sh
read init
read msg
echo '{"type":"control_request","request_id":"w","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"a.txt","content":"x"}}}'
read a
echo "$a" > write.json
echo '{"type":"control_request","request_id":"g","request":{"subtype":"can_use_tool","tool_name":"Grep","input":{"pattern":"pay"}}}'
read b
echo "$b" > grep.json
echo '{"type":"result","subtype":"success","duration_ms":10}'
read more
`), 0o755)
	s := Start(Options{ID: "ask-1", Worktree: dir, Command: bin, Prompt: "де оплата?", Tools: []string{"Read", "Grep", "Glob"}})
	defer s.Stop()
	eventually(t, func() bool { st, _, _ := s.State(); return st == "done" }, func() string { return fmt.Sprintf("%+v", s.Snapshot()) })
	if b, _ := os.ReadFile(filepath.Join(dir, "write.json")); !strings.Contains(string(b), `"behavior":"deny"`) || !strings.Contains(string(b), "лише читати") {
		t.Fatalf("write answered %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "grep.json")); !strings.Contains(string(b), `"behavior":"allow"`) {
		t.Fatalf("grep answered %s", b)
	}
	if s.Asked() != nil {
		t.Fatal("asked the developer")
	}
	kinds := []string{}
	for _, it := range s.Snapshot().Items {
		kinds = append(kinds, it.Kind+":"+it.Status+":"+it.Tool)
	}
	if strings.Join(kinds, " ") != "user:: permission:denied:Write result::" {
		t.Fatalf("items %v", kinds)
	}
}
