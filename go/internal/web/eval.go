package web

import (
	"context"
	"net/http"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// evalBodyMax: a variant, its CLAUDE.md (at most 256 KB) and the end of the prompt with it.
const evalBodyMax = 320 << 10

// evalRoutes: the eval of a repository (key: tracker/project/repo) in the stats of its project: a merged task added
// to its set or removed from it, a run started with a variant or stopped.
func (s *Server) evalRoutes(mux *http.ServeMux) {
	answer := func(w http.ResponseWriter, err error, extra map[string]any) {
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		reply := map[string]any{"ok": true}
		for k, v := range extra {
			reply[k] = v
		}
		writeJSON(w, reply)
	}
	mux.HandleFunc("POST /api/eval/add", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		answer(w, s.D.AddEval(context.WithoutCancel(r.Context()), r.URL.Query().Get("id")), nil)
	})
	mux.HandleFunc("POST /api/eval/remove", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		answer(w, s.D.RemoveEval(q.Get("key"), q.Get("id")), nil)
	})
	// The body is the variant and the ceiling: {"variant": {...}, "maxCost": 10}; an empty variant is the baseline.
	// Mac only: the variant's CLAUDE.md may be read from a file of this Mac.
	mux.HandleFunc("POST /api/eval/run", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Variant core.EvalVariant `json:"variant"`
			MaxCost float64          `json:"maxCost"`
		}
		if err := decodeJSON(r, evalBodyMax, &in); err != nil {
			answer(w, err, nil)
			return
		}
		id, err := s.D.StartEval(context.WithoutCancel(r.Context()), r.URL.Query().Get("key"), in.Variant, in.MaxCost)
		answer(w, err, map[string]any{"id": id})
	})
	mux.HandleFunc("POST /api/eval/stop", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		answer(w, s.D.StopEval(r.URL.Query().Get("key")), nil)
	})
}
