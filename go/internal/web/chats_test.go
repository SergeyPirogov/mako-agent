package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
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
	// A plan handed over to a new chat needs a project; without one no chat is made.
	if _, out := post("/api/chats/new", `{"project":"","text":"План","fromPlan":true}`); out["ok"] != false || out["error"] != "choose a project" || len(d.FreeChats()) != 0 {
		t.Fatalf("new from a plan with no project %v %+v", out, d.FreeChats())
	}
	if _, out := post("/api/chats/remove?id=chat-abc", ""); out["ok"] != false {
		t.Fatalf("remove %v", out)
	}
}

// «Chats» lists the chats and the questions about code in one list, the latest first, with their kind; «×» removes
// either kind.
func TestChatsListWithQuestions(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	state := t.TempDir()
	d := dispatch.New(&config.Config{StateDir: state}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	os.WriteFile(filepath.Join(state, "chats.json"), []byte(`[
	  {"id":"chat-a","project":"","title":"chat old","at":10},
	  {"id":"chat-b","project":"","title":"chat new","at":30}]`), 0o600)
	os.WriteFile(filepath.Join(state, "questions.json"), []byte(`[
	  {"id":"ask-a","project":"t/p/r","title":"ask mid","at":20,"ticket":"T-1"},
	  {"id":"ask-b","project":"t/q/r","title":"ask first","at":40}]`), 0o600)

	list := func() []map[string]any {
		t.Helper()
		res, err := http.Get(srv.URL + "/api/chats")
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Chats []map[string]any }
		json.NewDecoder(res.Body).Decode(&out)
		return out.Chats
	}
	ids := func() string {
		var b strings.Builder
		for _, c := range list() {
			b.WriteString(c["kind"].(string) + ":" + c["id"].(string) + " ")
		}
		return strings.TrimSpace(b.String())
	}
	if got := ids(); got != "ask:ask-b chat:chat-b ask:ask-a chat:chat-a" {
		t.Fatalf("list %q", got)
	}
	if c := list()[2]; c["ticket"] != "T-1" || c["project"] != "t/p/r" || c["title"] != "ask mid" || c["at"] != float64(20) {
		t.Fatalf("ask line %v", c)
	}
	remove := func(id string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/api/chats/remove?id="+id, nil)
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if out := remove("ask-a"); out["ok"] != true {
		t.Fatalf("remove ask %v", out)
	}
	if out := remove("ask-a"); out["ok"] != false {
		t.Fatalf("removed twice %v", out)
	}
	if got := ids(); got != "ask:ask-b chat:chat-b chat:chat-a" {
		t.Fatalf("after removing a question %q", got)
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

// pairPhone: a phone paired through `tailscale serve`; its requests carry the device's cookie it returns.
func pairPhone(t *testing.T, srv *httptest.Server, phones *phone.Phones) *http.Cookie {
	t.Helper()
	pair, _ := http.NewRequest("POST", srv.URL+"/pair", strings.NewReader("code="+phones.NewPairing()))
	pair.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pair.Header.Set("X-Forwarded-For", "100.64.0.2")
	pair.Header.Set("X-Forwarded-Proto", "https")
	pair.Header.Set("Origin", srv.URL)
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(pair)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Cookies() {
		if c.Name == deviceCookie {
			return c
		}
	}
	t.Fatalf("the phone did not pair: %d", res.StatusCode)
	return nil
}

// A plan handed over to another session is from the Mac only; the phone still starts and answers chats of its own.
func TestPlanHandoverIsFromTheMacOnly(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	srv := httptest.NewServer((&Server{D: d, Version: "test", Phones: phones}).Handler())
	defer srv.Close()
	device := pairPhone(t, srv, phones)
	post := func(path, body string, phone bool) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Origin", srv.URL)
		if phone {
			req.Header.Set("X-Forwarded-For", "100.64.0.2")
			req.AddCookie(device)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	plan := "Execute this plan: Cache\n\n1. Add it\n\n_Plan from task grp/app#3._"
	body, _ := json.Marshal(map[string]any{"project": "nope", "text": plan, "fromPlan": true})
	// From the Mac the text reaches NewChat (this project has no clone, so it answers with that).
	if code, out := post("/api/chats/new", string(body), false); code != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), "no clone") {
		t.Fatalf("new from the Mac %d %v", code, out)
	}
	cases := []struct{ path, body string }{
		{"/api/chats/new", string(body)},
		{"/api/create", `{"tracker":"t","project":"grp/app","title":"Cache","description":"1. Add it","fromPlan":true}`},
		{"/api/say?id=chat-abc&plan=1", plan},
	}
	for _, c := range cases {
		if code, _ := post(c.path, c.body, true); code != http.StatusForbidden {
			t.Errorf("%s from a phone: %d", c.path, code)
		}
	}
	// Without the plan the phone's requests are as before: not refused (the chat does not exist, so ok is false).
	if code, out := post("/api/say?id=chat-abc", "Привіт", true); code == http.StatusForbidden {
		t.Errorf("a phone's message to a chat: %d %v", code, out)
	}
	plain, _ := json.Marshal(map[string]any{"project": "nope", "text": "Привіт"})
	if code, _ := post("/api/chats/new", string(plain), true); code == http.StatusForbidden {
		t.Errorf("a phone's new chat: %d", code)
	}
}

// «Continue after the reset» of a chat the Claude limit cut: the choice reaches the session (a free chat's, a
// question's or a task's alike), the snapshot tells when the limit resets, and a chat with no agent says so.
func TestResumeOnResetRoute(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	post := func(path string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+path, nil)
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if out := post("/api/resumeonreset?id=chat-none&on=1"); out["ok"] != false || !strings.Contains(out["error"].(string), "no such chat") {
		t.Fatalf("no chat: %v", out)
	}
	reset := time.Now().Add(time.Hour).Unix()
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fmt.Sprintf(`#!/bin/sh
read init
read msg
echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":%d}}'
echo '{"type":"result","subtype":"success","is_error":true,"result":"You have hit your limit","duration_ms":10}'
sleep 30
`, reset)), 0o755)
	s := chat.Start(chat.Options{ID: "chat-l", Worktree: t.TempDir(), Command: bin, Prompt: "Зроби"})
	d.Chats.Put(s)
	defer s.Stop()
	for deadline := time.Now().Add(10 * time.Second); s.Snapshot().State != "limited"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("not limited: %+v", s.Snapshot())
		}
	}
	snapshot := func() chat.Snapshot {
		res, err := http.Get(srv.URL + "/api/chat?id=chat-l")
		if err != nil {
			t.Fatal(err)
		}
		var snap chat.Snapshot
		json.NewDecoder(res.Body).Decode(&snap)
		return snap
	}
	if snap := snapshot(); snap.State != "limited" || snap.LimitedUntil != reset || snap.ResumeOnReset {
		t.Fatalf("snapshot %q until %d, resume %v", snap.State, snap.LimitedUntil, snap.ResumeOnReset)
	}
	if out := post("/api/resumeonreset?id=chat-l&on=1"); out["ok"] != true {
		t.Fatalf("on: %v", out)
	}
	if snap := snapshot(); !snap.ResumeOnReset {
		t.Fatal("the choice did not reach the session")
	}
	if out := post("/api/resumeonreset?id=chat-l&on=0"); out["ok"] != true || snapshot().ResumeOnReset {
		t.Fatalf("off: %v", out)
	}
	// A task cut before Mako restarted (no agent running): the choice is kept with it, and the chat shows the
	// limit kept.
	store.Update("grp/app#9", func(r *core.Record) { r.Status, r.LimitedUntil, r.ResumeAfterLimit = core.HandedOff, reset, true })
	if out := post("/api/resumeonreset?id=" + url.QueryEscape("grp/app#9") + "&on=0"); out["ok"] != true {
		t.Fatalf("no agent: %v", out)
	}
	if r, _ := store.Get("grp/app#9"); r.ResumeAfterLimit || r.LimitedUntil != reset {
		t.Fatalf("kept %+v", r)
	}
	res, err := http.Get(srv.URL + "/api/chat?id=" + url.QueryEscape("grp/app#9"))
	if err != nil {
		t.Fatal(err)
	}
	var snap chat.Snapshot
	json.NewDecoder(res.Body).Decode(&snap)
	if snap.State != "limited" || snap.LimitedUntil != reset || snap.ResumeOnReset {
		t.Fatalf("snapshot %+v", snap)
	}
}
