package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// The chats without a task: listed, and their controls go to the chat, not to a task Mako does not have.
func TestFreeChatRoutes(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	res, _ := http.Get(srv.URL + "/api/chats")
	var list struct{ Chats []dispatch.FreeChat }
	if json.NewDecoder(res.Body).Decode(&list); list.Chats == nil || len(list.Chats) != 0 {
		t.Fatalf("chats %+v", list)
	}
	post := func(path, body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	// Not "no such task": the chat answers itself.
	if code, out := post("/api/say?id=chat-abc", "Привіт"); code != 200 || out["ok"] != false || out["error"] != "no such chat" {
		t.Fatalf("say %d %v", code, out)
	}
	if code, out := post("/api/interrupt?id=chat-abc", ""); code != 200 || out["error"] != "the agent is not running" {
		t.Fatalf("interrupt %d %v", code, out)
	}
	if code, _ := post("/api/take?id=chat-abc", ""); code != 400 {
		t.Fatalf("take %d", code)
	}
	if _, out := post("/api/chats/new", `{"project":"nope","text":"Привіт"}`); out["ok"] != false {
		t.Fatalf("new %v", out)
	}
	if _, out := post("/api/chats/remove?id=chat-abc", ""); out["ok"] != false {
		t.Fatalf("remove %v", out)
	}
}

// The plan of a chat without a task: sent back and approved in the chat itself; no task is made of the chat.
func TestFreeChatPlan(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(`#!/bin/sh
read init
read msg
echo '{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Зробити"}}}'
read answer
echo "$answer" > revise.json
echo '{"type":"control_request","request_id":"p2","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Зробити все"}}}'
read answer
echo "$answer" > reject.json
echo '{"type":"control_request","request_id":"p3","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Зробити й протестувати"}}}'
read answer
echo "$answer" > approve.json
read mode
echo "$mode" > mode.json
read notes
echo "$notes" > notes.json
read more
`), 0o755)
	s := chat.Start(chat.Options{ID: "chat-p", Worktree: dir, Command: bin, Prompt: "Зроби", Mode: "plan"})
	d.Chats.Put(s)
	defer s.Stop()
	asked := func(id string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); s.Asked() == nil || s.Asked().ID != id; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no plan %s", id)
			}
		}
	}
	read := func(name, want string) string {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			b, _ := os.ReadFile(filepath.Join(dir, name))
			if strings.Contains(string(b), want) {
				return string(b)
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s", name, b)
			}
		}
	}
	plan := func(query, body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/api/plan?id=chat-p&"+query, strings.NewReader(body))
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	asked("p1")
	// Revised: the agent reads the notes as text, not the request's JSON.
	if out := plan("req=p1", `{"do":"revise","notes":{"steps":[{"n":1,"title":"Зробити","note":"Додай тести"}]}}`); out["ok"] != true {
		t.Fatalf("revise %v", out)
	}
	if b := read("revise.json", `"behavior":"deny"`); !strings.Contains(b, "Step 1 (Зробити): Додай тести") || strings.Contains(b, `\"steps\"`) {
		t.Fatalf("revise %s", b)
	}
	// Rejected: turned down, not sent back for changes.
	asked("p2")
	if out := plan("req=p2", `{"do":"reject"}`); out["ok"] != true {
		t.Fatalf("reject %v", out)
	}
	if b := read("reject.json", `"behavior":"deny"`); strings.Contains(b, "Додай тести") {
		t.Fatalf("reject %s", b)
	}
	if st := s.Snapshot(); !planStatus(st, "p2", "denied") {
		t.Fatalf("p2 not rejected: %+v", st.Items)
	}
	// Approved in the mode picked.
	asked("p3")
	if out := plan("req=p3", `{"do":"approve","mode":"acceptEdits","notes":{"whole":"Обережно з міграціями"}}`); out["ok"] != true {
		t.Fatalf("approve %v", out)
	}
	read("approve.json", `"behavior":"allow"`)
	read("mode.json", `"acceptEdits"`)
	read("notes.json", "Обережно з міграціями") // the notes come as your message once it goes on
	if out := plan("req=p3", `{"do":"nope"}`); out["ok"] != false {
		t.Fatalf("unknown action %v", out)
	}
	if _, ok := store.Get("chat-p"); ok {
		t.Fatal("approving the plan made a task of the chat")
	}
}

// planStatus: the plan request's item in the conversation has the status.
func planStatus(s chat.Snapshot, req, status string) bool {
	for _, it := range s.Items {
		if it.ID == req {
			return it.Status == status
		}
	}
	return false
}
