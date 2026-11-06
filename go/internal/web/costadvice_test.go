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

// A suggestion as the Cost tab shows it: the routes compared with the one in use marked, and a link to the routes'
// table for one about a route; text from the server is escaped.
func TestBoardAdviceItem(t *testing.T) {
	runNodeScript(t, nodeEq+cutBlock(t, board, "advice")+`
const a = { kind: "warn", title: "Big tasks · Work: Opus · high is cheaper", text: "about $107.87 less", phase: "work", size: "big", set: { model: "opus", effort: "high" },
  rows: [{ label: "Sonnet · high", figures: "$24.46 a task", now: true }, { label: "Opus · high", figures: "$9.05 a task" }] };
const h = adviceItemHTML(a);
eq(h.includes("Sonnet · high <span class=\"muted\">(now)</span>") && !h.includes("Opus · high <span"), true, "the route in use marked");
eq(h.includes('href="/settings#routes">Change it in Settings →'), true, "a link to the table");
eq(adviceItemHTML({ kind: "tip", title: "Big tasks", text: "split" }).includes("/settings"), false, "no link for a tip");
eq(adviceItemHTML({ kind: "wait", title: "now <i>", text: "", phase: "work" }).includes("now <i>"), false, "escaped");
`)
}

// Settings → Models and effort: a suggestion with the route in use marked, and the label of the route its button
// puts in.
func TestSettingsAdvice(t *testing.T) {
	runNodeScript(t, nodeEq+`const t = (k, v = {}) => ({ "s.r.adviceNow": "(now)" }[k] ?? k);
`+cutBlock(t, settingsPage, "advice")+`
const h = adviceHTML({ kind: "warn", title: "Big <x>", text: "t", rows: [{ label: "Sonnet · high", figures: "$24", now: true }] });
eq(h.includes("Sonnet · high <span class=\"muted\">(now)</span>") && !h.includes("Big <x>"), true, "marked and escaped");
eq(routeLabelOf({ model: "opus", effort: "high" }), "Opus · high", "the button's route");
eq(routeLabelOf({ effort: "max" }), "max", "effort only");
`)
}

// GET /api/settings/advice: the suggestions over the last 30 days, a list even when there are none.
func TestSettingsAdviceEndpoint(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: t.TempDir()}).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/settings/advice")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK     bool          `json:"ok"`
		Days   int           `json:"days"`
		Advice []core.Advice `json:"advice"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || !out.OK || out.Days != 30 || out.Advice == nil {
		t.Fatalf("%+v %v", out, err)
	}
}
