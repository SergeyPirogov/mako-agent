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
echo '{"type":"control_request","request_id":"p2","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Зробити й протестувати"}}}'
read answer
echo "$answer" > approve.json
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
	if out := plan("req=p1&do=revise", "Додай тести"); out["ok"] != true {
		t.Fatalf("revise %v", out)
	}
	asked("p2")
	if out := plan("req=p2&do=approve", ""); out["ok"] != true {
		t.Fatalf("approve %v", out)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(filepath.Join(dir, "approve.json")); strings.Contains(string(b), `"behavior":"allow"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the plan was not approved")
		}
	}
	if _, ok := store.Get("chat-p"); ok {
		t.Fatal("approving the plan made a task of the chat")
	}
}
