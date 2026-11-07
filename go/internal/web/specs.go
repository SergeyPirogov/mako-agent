package web

import (
	"context"
	"net/http"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
)

// specBodyMax bounds a request about an idea: its text, or its specification and tasks as edited on the board.
const specBodyMax = 256 << 10

// specRoutes: «New idea»: the idea («new»), the answers to the agent's questions («answer»), «Try again»
// («retry»), «Create in tracker» («create») and «Drop» («drop»). Mac only (macOnly): the agent reads the clone of
// this Mac and spends the budget.
func (s *Server) specRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/specs/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			ID      string            `json:"id"`
			Tracker string            `json:"tracker"`
			Project string            `json:"project"`
			Text    string            `json:"text"`
			Images  []string          `json:"images"`
			Answers []dispatch.Answer `json:"answers"`
			Title   string            `json:"title"`
			Body    string            `json:"body"`
			Items   []core.SplitItem  `json:"items"`
		}
		if err := decodeJSON(r, specBodyMax, &in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		reply := map[string]any{"ok": true}
		var err error
		switch r.PathValue("action") {
		case "new":
			var id string
			id, err = s.D.NewSpec(in.Tracker, in.Project, in.Text, in.Images)
			reply["id"] = id
		case "answer":
			err = s.D.AnswerSpec(in.ID, in.Answers)
		case "retry":
			err = s.D.RetrySpec(in.ID)
		case "create":
			err = s.D.CreateSpec(context.WithoutCancel(r.Context()), in.ID, in.Title, in.Body, in.Items)
		case "drop":
			err = s.D.DropSpec(in.ID)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, reply)
	})
}
