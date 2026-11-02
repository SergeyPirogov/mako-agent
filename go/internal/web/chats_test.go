package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
