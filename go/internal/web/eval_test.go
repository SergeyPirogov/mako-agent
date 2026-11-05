package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// The stats of a project carry its eval sets and runs; the eval's buttons answer {ok:false} with why when they
// cannot do it, and a run is not started from a phone.
func TestEvalRoutes(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	dir := t.TempDir()
	sets := map[string]dispatch.EvalSet{"gl/grp/app/app": {TrackerID: "gl", Project: "grp/app", Repo: "app",
		Cases: []core.EvalCase{{TaskID: "grp/app#5", Task: core.Task{ID: "grp/app#5", Title: "Pay"}, Base: "b", Head: "h",
			Tests: []core.EvalFile{{Path: "pay_test.go", Content: "package app"}}}},
		Runs: []core.EvalRun{{ID: "r1", State: core.EvalDone, Results: []core.EvalResult{{TaskID: "grp/app#5", State: core.EvalPassed, Cost: 1}}}}}}
	b, _ := json.Marshal(sets)
	os.WriteFile(filepath.Join(dir, "evals.json"), b, 0o600)
	d := dispatch.New(&config.Config{StateDir: dir}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/state?project=grp%2Fapp")
	if err != nil {
		t.Fatal(err)
	}
	var st stateReply
	json.NewDecoder(res.Body).Decode(&st)
	res.Body.Close()
	if len(st.Evals) != 1 || st.Evals[0].Key != "gl/grp/app/app" || len(st.Evals[0].Cases) != 1 || len(st.Evals[0].Runs) != 1 {
		t.Fatalf("evals %+v", st.Evals)
	}
	if c := st.Evals[0].Cases[0]; c.Tests != nil || len(c.TestFiles) != 1 {
		t.Errorf("the board gets the tests' contents: %+v", c)
	}
	if r := st.Evals[0].Runs[0]; !r.Baseline || r.Summary.SuccessRate != 1 {
		t.Errorf("run %+v", r)
	}
	if res, _ := http.Get(srv.URL + "/api/state?project=grp%2Fother"); res != nil {
		var other stateReply
		json.NewDecoder(res.Body).Decode(&other)
		res.Body.Close()
		if len(other.Evals) != 0 {
			t.Errorf("another project's evals: %+v", other.Evals)
		}
	}

	post := func(path, body string, phone bool) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Origin", srv.URL)
		if phone {
			req.Header.Set("X-Forwarded-For", "100.64.0.2")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	for _, c := range []struct{ path, body, want string }{
		{"/api/eval/add?id=grp%2Fapp%2399", "", "no such task"},
		{"/api/eval/remove?key=gl%2Fgrp%2Fapp%2Fapp&id=grp%2Fapp%2399", "", "not in the eval set"},
		{"/api/eval/run?key=nope", `{"variant":{}}`, "empty"},
		{"/api/eval/run?key=gl%2Fgrp%2Fapp%2Fapp", `{"variant":{"model":"--x"}}`, "not a model"},
		{"/api/eval/run?key=nope", `{"variant":`, "could not read"},
		{"/api/eval/stop?key=gl%2Fgrp%2Fapp%2Fapp", "", "no eval run"},
	} {
		code, out := post(c.path, c.body, false)
		if got, _ := out["error"].(string); code != 200 || out["ok"] != false || !strings.Contains(got, c.want) {
			t.Errorf("%s: %d %v", c.path, code, out)
		}
	}
	if code, _ := post("/api/eval/run?key=gl%2Fgrp%2Fapp%2Fapp", `{"variant":{}}`, true); code == 200 {
		t.Error("an eval run started from a phone")
	}
	if !macOnly("/api/eval/run") || macOnly("/api/eval/add") {
		t.Error("only starting a run is Mac-only")
	}
}
