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
const rc = { models: ["haiku", "sonnet", "opus"], efforts: ["low", "medium", "high", "xhigh", "max"] };
eq(usable({ model: "opus", effort: "high" }, rc), true, "a route the table offers");
eq(usable({ model: "—", effort: "high" }, rc) || usable({ model: "gpt", effort: "high" }, rc) || usable({ model: "opus" }, rc) || usable(undefined, rc), false, "one it does not");
`)
}

// Settings → Models and effort, the table painted: each cell a suggestion is about is marked; the «Use …» button is
// there only for a route the selects offer, and puts it in; a cell that holds the suggested route already is left
// alone.
func TestSettingsAdviceButtons(t *testing.T) {
	runNodeScript(t, nodeEq+`const t = (k, v = {}) => k === "s.r.adviceUse" ? "Use " + v.route : k;
const routeChoices = { models: ["haiku", "sonnet", "opus"], efforts: ["low", "medium", "high", "xhigh", "max"] };
const el = (tag) => ({ tag, kids: [], classes: new Set(), classList: { add(...c) { c.forEach((x) => this.o.classes.add(x)); }, remove(...c) { c.forEach((x) => this.o.classes.delete(x)); } },
  append(x) { this.kids.push(x); }, remove() { this.gone = true; }, querySelector() { return null; } });
const mk = (tag) => { const e = el(tag); e.classList.o = e; return e; };
const document = { createElement: mk };
const cells = {};
for (const s of ["small", "medium", "big"]) {
  const td = mk("td"), sels = {};
  for (const k of ["model", "effort"]) sels[k] = { value: "", changed: 0, onchange() { this.changed++; }, closest: () => td };
  if (s === "small") Object.assign(sels.model, { value: "opus" }), Object.assign(sels.effort, { value: "high" });
  td.querySelector = (q) => sels[q.match(/data-k="(\w+)"/)[1]];
  cells[s] = { td, sels };
}
const wrap = { querySelector: (q) => cells[q.match(/data-s="(\w+)"/)?.[1]]?.sels.model ?? null };
const box = { isConnected: true, querySelector: () => null, insertBefore() {} };
const fetch = async () => ({ json: async () => ({ ok: true, days: 30, advice: [
  { kind: "warn", title: "a", text: "", phase: "work", size: "medium", set: { model: "opus", effort: "high" } },
  { kind: "warn", title: "c", text: "", phase: "work", size: "small", set: { model: "opus", effort: "high" } },
  { kind: "warn", title: "b", text: "", phase: "work", size: "big", set: { model: "—", effort: "high" } }] }) });
`+cutBlock(t, settingsPage, "advice")+`
(async () => {
  await paintAdvice(box, wrap);
  const btn = (s) => cells[s].td.kids.filter((k) => k.className === "use-route");
  eq([cells.medium.td.classes.has("advise"), cells.big.td.classes.has("advise")], [true, true], "both cells marked");
  eq(btn("medium").map((b) => b.textContent), ["Use Opus · high"], "a button for a route the table offers");
  eq(btn("big").length, 0, "none for a model it does not");
  eq([cells.small.td.classes.has("advise"), btn("small").length], [false, 0], "none for a cell that holds the route");
  btn("medium")[0].onclick();
  eq([cells.medium.sels.model.value, cells.medium.sels.effort.value, cells.medium.sels.model.changed], ["opus", "high", 1], "the button puts the route in");
  eq(cells.medium.td.classes.has("advised"), true, "the cell marked as changed");
})();
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
