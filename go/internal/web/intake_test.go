package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// The board gets what it draws the intake from: whether the decision still stands («Take anyway», the status in
// the list) and the intake's stats; a comment since the decision lets it go.
func TestStateTellsIntake(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	task := core.Task{ID: "grp/app#5", Project: "grp/app", Title: "Кнопка", Description: "Падає"}
	clarify := dispatch.IntakeFor(task, core.Intake{At: "2026-11-01T00:00:00Z", State: "done", Decision: core.IntakeClarify, Why: "неясно", Questions: []string{"Що?"}})
	store.Update(task.ID, func(r *core.Record) { r.Task, r.Status, r.Intake = task, core.Asking, &clarify })
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	get := func() (bool, core.IntakeStats) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
		req.Header.Set("X-Mako", "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var got struct {
			Records []struct {
				ID           string `json:"id"`
				IntakeStands bool   `json:"intakeStands"`
			} `json:"records"`
			Stats struct {
				Intake core.IntakeStats `json:"intake"`
			} `json:"stats"`
		}
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil || len(got.Records) != 1 {
			t.Fatalf("%v, %+v", err, got)
		}
		return got.Records[0].IntakeStands, got.Stats.Intake
	}
	stands, st := get()
	if !stands || st.Ran != 1 || st.Clarify != 1 || st.Filtered != 1 {
		t.Fatalf("stands %v, stats %+v", stands, st)
	}
	store.Update(task.ID, func(r *core.Record) {
		r.Task.Comments = append(r.Task.Comments, core.Comment{Author: "Serg", Body: "На картці"})
	})
	if stands, _ = get(); stands {
		t.Fatal("still stands after an answer")
	}
}
