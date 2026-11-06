package web

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// «Розбити», «Створити», «Не розбивати»: what the board sends reaches the dispatcher, and what it says comes back.
func TestSplitActions(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	task := core.Task{ID: "grp/app#3", Title: "Велика", TrackerID: "gone"}
	store.Update(task.ID, func(r *core.Record) {
		r.Status, r.Task = core.Snoozed, task
		r.Breakdown = &core.Breakdown{State: "done", Items: []core.SplitItem{{Title: "A"}, {Title: "B"}}}
	})
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), Executor: "terminal"}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	post := func(action, body string) (reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}) {
		t.Helper()
		res, err := postMako(srv.URL+"/api/"+action+"?id="+url.QueryEscape(task.ID), "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		json.NewDecoder(res.Body).Decode(&reply)
		return reply
	}

	if r := post("splitcreate", `{"items":"не список"}`); r.OK || !strings.Contains(r.Error, "could not read the subtasks") {
		t.Errorf("bad body: %+v", r)
	}
	if r := post("splitcreate", `{"items":[{"title":"`+strings.Repeat("x", 70<<10)+`"}]}`); r.OK || !strings.Contains(r.Error, "could not read the subtasks") {
		t.Errorf("too long a body: %+v", r)
	}
	// The tracker of the task is gone: it cannot open the subtasks, and says so.
	if r := post("splitcreate", `{"items":[{"title":"A"},{"title":"B","after":[1]}]}`); r.OK || !strings.Contains(r.Error, "no tracker") {
		t.Errorf("create: %+v", r)
	}
	if r := post("split", ``); r.OK || !strings.Contains(r.Error, "no tracker") {
		t.Errorf("split: %+v", r)
	}
	if r := post("splitdrop", ``); !r.OK {
		t.Errorf("drop: %+v", r)
	}
	if r, _ := store.Get(task.ID); r.Breakdown != nil {
		t.Errorf("dropped, yet %+v", r.Breakdown)
	}
}
