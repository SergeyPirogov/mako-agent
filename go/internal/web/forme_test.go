package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// The buttons of «Рев'ю для мене»: from the board only, each answers what went wrong; the reviewer's spend is
// in the statistics as a phase of its own.
func TestReviewForMeRoutes(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	mr := "https://gl/grp/app/-/merge_requests/4"
	d.Reviews.Update(mr, func(r *core.ReviewRequest) { r.TrackerID, r.Project, r.State, r.URL = "gone", "grp/app", "open", mr })

	post := func(action, body, origin string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/review-request/"+action+"?id="+url.QueryEscape(mr), strings.NewReader(body))
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if code, _ := post("prepare", "", "https://evil.example"); code != 403 {
		t.Fatalf("cross-site: %d", code)
	}
	if code, _ := post("nope", "", srv.URL); code != 404 {
		t.Fatalf("unknown action: %d", code)
	}
	for action, body := range map[string]string{"prepare": "", "draft": `{"comment":"R1","text":"x"}`, "send": `{"ids":["R1"]}`, "approve": ""} {
		code, out := post(action, body, srv.URL)
		if code != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), "cannot show MRs") {
			t.Errorf("%s: %d %v", action, code, out)
		}
	}
	if _, out := post("send", "{", srv.URL); out["ok"] != false || !strings.Contains(out["error"].(string), "bad request") {
		t.Errorf("bad body: %v", out)
	}

	d.Spends.Add(core.Spend{At: time.Now().UTC().Format(time.RFC3339Nano), Phase: core.PhaseForMe, Project: "grp/app", USD: 0.3, Usage: core.Usage{Input: 40}})
	d.Spends.Add(core.Spend{At: time.Now().AddDate(0, 0, -30).UTC().Format(time.RFC3339Nano), Phase: core.PhaseForMe, Project: "grp/app", USD: 1})
	stats := func(q string) core.Stats {
		res, err := http.Get(srv.URL + "/api/state?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var st struct {
			Stats core.Stats `json:"stats"`
		}
		json.NewDecoder(res.Body).Decode(&st)
		return st.Stats
	}
	if st := stats(""); st.Phases[core.PhaseForMe] != 1.3 || st.PhaseTokens[core.PhaseForMe] != 40 {
		t.Fatalf("all time: %+v %+v", st.Phases, st.PhaseTokens)
	}
	if st := stats("days=7&project=grp/app"); st.Phases[core.PhaseForMe] != 0.3 {
		t.Fatalf("7 days: %+v", st.Phases)
	}
	if st := stats("project=acme/web"); st.Phases[core.PhaseForMe] != 0 {
		t.Fatalf("another project: %+v", st.Phases)
	}
}

// The card of an MR to review has Mako's draft: «Prepare review», the comments to edit, strike out and send,
// «Approve»; a comment being typed is not drawn over; the statistics name the phase.
func TestBoardReviewForMe(t *testing.T) {
	src := string(board)
	for _, want := range []string{
		"${draftHTML(q, card)}", `id="rq-prepare"`, `id="rq-send"`, `id="rq-approve"`, `class="dc-text"`, `class="dc-pick"`, `class="ghost dc-drop"`,
		"/api/review-request/${action}", `forme: "review for me"`, `No local clone of ${esc(q.project)}: the reviewer read the diff only.`,
		`document.activeElement?.classList.contains("dc-text")`, "bindDraft(q);",
		"if (dr.stale) parts.push(", `q.draft?.stale ?`, "changed since review", "will be replaced by the new review's; the sent ones stay.",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("board.html lacks %s", want)
		}
	}
	// Sending and approving are from me, in the MR: both ask first.
	for _, want := range []string{"to ${q.ref} as you?", "Approve ${q.ref} as you?"} {
		if !strings.Contains(src, "confirm(`"+want) && !strings.Contains(src, want) {
			t.Errorf("no confirmation %q", want)
		}
	}
	if !strings.Contains(string(style), ".dc.dropped .rm-t { text-decoration: line-through; }") {
		t.Error("a struck-out comment is not struck out")
	}
}
