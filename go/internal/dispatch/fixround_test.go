package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// A task past its MR (Done) whose agent in Mako's chat runs again, fixing the review's remarks, and asks for a
// permission: the board shows it waiting for you, as for a task in work. It used to stay unwatched: no
// «waiting for you», no notification, and the request sat unseen.
func TestFixRoundAgentWaitingIsWatched(t *testing.T) {
	d, _, _ := setup(t)
	wt, bin := t.TempDir(), filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(`#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s-1","model":"claude-opus-5-5"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"git push origin fix/1"}}}'
read answer
`), 0o755)
	d.Store.Update("t#1", func(r *core.Record) {
		r.Worktree, r.Executor, r.View, r.Status, r.PRURL = wt, terminalExecutor, "chat", core.Done, "https://example.com/mr/1"
	})
	s := chat.Start(chat.Options{ID: "t#1", Worktree: wt, Command: bin, Prompt: "Виправ зауваження",
		OnState: func() { d.watchTerminals(time.Now()) }})
	defer s.Stop()
	d.Chats.Put(s)

	waitFor(t, func() bool { a, ok := d.Agents()["t#1"]; return ok && a.State == "waiting" }, "the fix round's agent is not shown waiting")
	if a := d.Agents()["t#1"]; !strings.HasPrefix(a.Reason, "permission for Bash") {
		t.Errorf("reason %q", a.Reason)
	}
	// The poll keeps it too, it does not drop the agent of a task that is not in work.
	d.WatchAgents(context.Background(), time.Now())
	if a, ok := d.Agents()["t#1"]; !ok || a.State != "waiting" {
		t.Fatalf("after the poll: %+v %v", a, ok)
	}

	// Its agent gone: a task past its MR is not watched any more.
	s.Stop()
	waitFor(t, func() bool { return !s.Running() }, "the agent did not stop")
	d.WatchAgents(context.Background(), time.Now())
	if a, ok := d.Agents()["t#1"]; ok {
		t.Fatalf("still watched: %+v", a)
	}
}
