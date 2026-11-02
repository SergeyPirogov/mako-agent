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
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// A fake Claude Code that offers a plan and writes down everything it is sent after it.
const planClaude = `#!/bin/sh
read init
read msg
echo '{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Do it"}}}'
while read line; do echo "$line" >> sent.txt; done
`

// The plan is approved the way a notification's button does it (?do=approve, no body) and the way the board does
// it (a JSON body).
func TestPlanAction(t *testing.T) {
	for _, c := range []struct{ name, query, body string }{
		{"notification", "&do=approve", ""},
		{"board", "", `{"do":"approve","mode":"acceptEdits","notes":{"steps":[{"n":1,"title":"Do it","note":"and test it"}]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, wt := t.TempDir(), t.TempDir()
			store, _ := state.Open(dir)
			id := "grp/app#7"
			store.Update(id, func(r *core.Record) { r.Planning, r.Task = true, core.Task{ID: id, Title: "Plan", Project: "grp/app"} })
			d := dispatch.New(&config.Config{StateDir: dir}, store, nil)
			bin := filepath.Join(t.TempDir(), "claude")
			os.WriteFile(bin, []byte(planClaude), 0o755)
			s := chat.Start(chat.Options{ID: id, Worktree: wt, Command: bin, Prompt: "Plan it", Mode: "plan"})
			defer s.Stop()
			d.Chats.Put(s)
			srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
			defer srv.Close()
			for end := time.Now().Add(5 * time.Second); s.Asked() == nil; time.Sleep(20 * time.Millisecond) {
				if time.Now().After(end) {
					t.Fatal("no plan asked")
				}
			}
			res, err := http.Post(srv.URL+"/api/plan?id=grp%2Fapp%237&req=p1"+c.query, "application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			json.NewDecoder(res.Body).Decode(&reply)
			if !reply.OK {
				t.Fatalf("reply %+v", reply)
			}
			var sent string
			for end := time.Now().Add(5 * time.Second); !strings.Contains(sent, "set_permission_mode") && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
				b, _ := os.ReadFile(filepath.Join(wt, "sent.txt"))
				sent = string(b)
			}
			if !strings.Contains(sent, `"behavior":"allow"`) || !strings.Contains(sent, "set_permission_mode") {
				t.Fatalf("sent %s", sent)
			}
			if r, _ := store.Get(id); r.Planning {
				t.Fatal("still planning")
			}
			if c.body != "" {
				for end := time.Now().Add(5 * time.Second); !strings.Contains(sent, "and test it") && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
					b, _ := os.ReadFile(filepath.Join(wt, "sent.txt"))
					sent = string(b)
				}
				if !strings.Contains(sent, `"acceptEdits"`) || !strings.Contains(sent, "Step 1 (Do it): and test it") {
					t.Fatalf("sent %s", sent)
				}
			}
		})
	}
}

// The board shows the message with your notes on an approved plan as the notes: it knows how chat begins it.
func TestBoardKnowsPlanNotesPrefix(t *testing.T) {
	if !strings.Contains(string(board), `const PLAN_NOTES = "`+chat.PlanNotesPrefix+`";`) {
		t.Fatal("board.html's PLAN_NOTES is not chat.PlanNotesPrefix")
	}
}
