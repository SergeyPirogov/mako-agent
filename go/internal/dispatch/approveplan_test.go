package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// A fake Claude Code in plan mode: it offers a plan and writes down everything it is sent after it.
const approveClaude = `#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s1","model":"sonnet"}'
echo '{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Do it"}}}'
while read line; do echo "$line" >> sent.txt; done
`

// The same, but it offers the plan again (p2) once its first one (p1) is sent back.
const reviseClaude = `#!/bin/sh
read init
read msg
echo '{"type":"system","subtype":"init","session_id":"s1","model":"sonnet"}'
echo '{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Do it"}}}'
read answer
echo '{"type":"control_request","request_id":"p2","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"1. Do it, with tests"}}}'
while read line; do echo "$line" >> sent.txt; done
`

// Approving the plan: an unknown mode is refused and the plan stays asked; "plan" or none is the configured work
// mode; a model picked goes to the agent and becomes the work phase's route, its effort the plan's.
func TestApprovePlanModeAndModel(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	id := "grp/app#9"
	store.Update(id, func(r *core.Record) {
		r.Planning, r.Task = true, core.Task{ID: id, Title: "Plan"}
		r.AgentRoute = &core.Route{Model: "sonnet", Effort: "high", Why: "big task"}
	})
	d := New(&config.Config{StateDir: t.TempDir(), Terminal: config.Terminal{PermissionMode: "acceptEdits"}}, store, nil)
	wt, bin := t.TempDir(), filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(approveClaude), 0o755)
	c := chat.Start(chat.Options{ID: id, Worktree: wt, Command: bin, Prompt: "Plan it", Mode: "plan"})
	defer c.Stop()
	d.Chats.Put(c)
	waitFor(t, func() bool { return c.Asked() != nil && c.Model() == "sonnet" }, "no plan asked")

	if err := d.ApprovePlan(id, "p1", PlanApproval{Mode: "yolo"}); err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("unknown mode: %v", err)
	}
	if c.Asked() == nil {
		t.Fatal("the plan was answered on an unknown mode")
	}
	if err := d.ApprovePlan(id, "p1", PlanApproval{Mode: "plan", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	read := func() string { b, _ := os.ReadFile(filepath.Join(wt, "sent.txt")); return string(b) }
	waitFor(t, func() bool { return strings.Contains(read(), "set_model") }, "no model sent: "+read())
	if sent := read(); !strings.Contains(sent, `"mode":"acceptEdits"`) || !strings.Contains(sent, `"model":"opus"`) {
		t.Fatalf("sent %s", sent)
	}
	r, _ := store.Get(id)
	want := core.Route{Model: "opus", Effort: "high", Why: "picked when the plan was approved"}
	if r.Planning || r.AgentModel != "opus" || r.AgentRoute == nil || *r.AgentRoute != want || r.RouteOf(core.PhaseWork) != want {
		t.Fatalf("record planning=%v model=%q route=%+v work=%+v", r.Planning, r.AgentModel, r.AgentRoute, r.RouteOf(core.PhaseWork))
	}
}

// With no mode picked the plan's work goes on in the configured one ("plan" in the settings means auto).
func TestApprovePlanDefaultMode(t *testing.T) {
	for _, c := range []struct{ setting, picked, want string }{{"", "", "auto"}, {"plan", "", "auto"}, {"acceptEdits", "", "acceptEdits"}, {"acceptEdits", "plan", "acceptEdits"}} {
		store, _ := state.Open(t.TempDir())
		id := "grp/app#10"
		store.Update(id, func(r *core.Record) { r.Planning, r.Task = true, core.Task{ID: id, Title: "Plan"} })
		d := New(&config.Config{StateDir: t.TempDir(), Terminal: config.Terminal{PermissionMode: c.setting}}, store, nil)
		wt, bin := t.TempDir(), filepath.Join(t.TempDir(), "claude")
		os.WriteFile(bin, []byte(approveClaude), 0o755)
		s := chat.Start(chat.Options{ID: id, Worktree: wt, Command: bin, Prompt: "Plan it", Mode: "plan"})
		d.Chats.Put(s)
		waitFor(t, func() bool { return s.Asked() != nil }, "no plan asked")
		if err := d.ApprovePlan(id, "p1", PlanApproval{Mode: c.picked}); err != nil {
			t.Fatal(err)
		}
		read := func() string { b, _ := os.ReadFile(filepath.Join(wt, "sent.txt")); return string(b) }
		waitFor(t, func() bool { return strings.Contains(read(), "set_permission_mode") }, "no mode sent")
		if !strings.Contains(read(), `"mode":"`+c.want+`"`) || strings.Contains(read(), "set_model") {
			t.Errorf("%+v: sent %s", c, read())
		}
		s.Stop()
	}
}

// The plan's stats: each «Поправити» is counted and the approval is stamped, for the route stats' «approved first
// time» and the time to approval. A revision the chat refuses (no notes, or the plan was already answered) is not
// counted.
func TestPlanRevisionsAndApprovalKept(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	id := "grp/app#11"
	store.Update(id, func(r *core.Record) { r.Planning, r.Task = true, core.Task{ID: id, Title: "Plan"} })
	d := New(&config.Config{StateDir: t.TempDir(), Terminal: config.Terminal{PermissionMode: "acceptEdits"}}, store, nil)
	wt, bin := t.TempDir(), filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(reviseClaude), 0o755)
	c := chat.Start(chat.Options{ID: id, Worktree: wt, Command: bin, Prompt: "Plan it", Mode: "plan"})
	defer c.Stop()
	d.Chats.Put(c)
	waitFor(t, func() bool { return c.Asked() != nil }, "no plan asked")

	if err := d.RevisePlan(id, "p1", " "); err == nil {
		t.Fatal("empty notes were sent")
	}
	if r, _ := store.Get(id); r.PlanRevisions != 0 {
		t.Fatalf("a refused revision counted: %d", r.PlanRevisions)
	}
	if err := d.RevisePlan(id, "p1", "Add tests"); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.Get(id); r.PlanRevisions != 1 || r.PlanApprovedAt != "" {
		t.Fatalf("after the revision: %d revisions, approved %q", r.PlanRevisions, r.PlanApprovedAt)
	}
	// The plan p1 is answered now: sending it back once more is refused by the chat, and is not counted.
	if err := d.RevisePlan(id, "p1", "Once more"); err == nil {
		t.Fatal("an answered plan was sent back")
	}
	if r, _ := store.Get(id); r.PlanRevisions != 1 {
		t.Fatalf("a revision of an answered plan counted: %d", r.PlanRevisions)
	}
	// The agent offers the plan again; approving it stamps the approval.
	waitFor(t, func() bool { a := c.Asked(); return a != nil && a.ID == "p2" }, "no plan asked again")
	if err := d.ApprovePlan(id, "p2", PlanApproval{}); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.Get(id); r.PlanApprovedAt == "" || r.PlanRevisions != 1 {
		t.Fatalf("after the approval: %d revisions, approved %q", r.PlanRevisions, r.PlanApprovedAt)
	}
}
