package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
)

// chatRoutes: the chats without a task — their list, opening one with its first message, and removing one. The
// conversation itself is /api/chat?id=<the chat's id>; the messages, answers and the rest go to /api/{action}, as a
// task's do (freeChatAction).
func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/chats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"chats": s.allChats()})
	})
	// A new chat: {project, text}. An empty project is a chat with no project.
	// fromPlan: the text is an agent's plan handed over from another session; it needs a project, and the phone
	// does not do it.
	mux.HandleFunc("POST /api/chats/new", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Project, Text string
			FromPlan      bool `json:"fromPlan"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 200<<10)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if in.FromPlan && remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if in.FromPlan && in.Project == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "choose a project"})
			return
		}
		id, err := s.D.NewChat(in.Project, in.Text)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	})
	mux.HandleFunc("POST /api/chats/remove", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id := r.URL.Query().Get("id")
		remove := s.D.RemoveFreeChat
		if dispatch.IsQuestion(id) {
			remove = s.D.RemoveQuestion
		}
		if err := remove(id); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// chatEntry: one line of the history «Chats»: a chat (kind "chat", all of FreeChat) or a question about code
// (kind "ask": id, project, title, at, ticket).
type chatEntry struct {
	Kind string `json:"kind"`
	dispatch.FreeChat
}

type askEntry struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Project string `json:"project"`
	Title   string `json:"title"`
	At      int64  `json:"at"`
	Ticket  string `json:"ticket,omitempty"`
}

// allChats: the chats and the questions in one list, the latest first.
func (s *Server) allChats() []any {
	type line struct {
		at int64
		v  any
	}
	var all []line
	for _, c := range s.D.FreeChats() {
		all = append(all, line{c.At, chatEntry{Kind: "chat", FreeChat: c}})
	}
	for _, t := range s.D.AllCodeThreads() {
		all = append(all, line{t.At, askEntry{"ask", t.ID, t.Project, t.Title, t.At, t.Ticket}})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at > all[j].at })
	out := make([]any, len(all))
	for i, l := range all {
		out[i] = l.v
	}
	return out
}

// freeChatAction: what the chat's controls do in a chat without a task (/api/{action}?id=chat-…).
func (s *Server) freeChatAction(w http.ResponseWriter, r *http.Request, action, id string) {
	var err error
	c := s.D.Chats.Get(id)
	switch action {
	case "say":
		// To a closed agent too: the message resumes its conversation.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 200<<10))
		err = s.D.SayFree(id, strings.TrimSpace(string(body)))
	case "resume":
		err = s.D.ResumeFree(id)
	case "stop":
		s.D.ForgetLimit(id) // stopped by you: it does not go on by itself after the limit either
		if c != nil {
			c.Stop()
		}
	case "plan":
		// Not the task's ApprovePlan: it would note the plan in the task store, making a task of the chat.
		var do string
		var o dispatch.PlanApproval
		if do, o, err = readPlan(r); err == nil {
			err = s.D.FreePlan(id, r.URL.Query().Get("req"), do, o)
		}
	case "permit", "answer", "choose", "interrupt", "mode":
		if action == "interrupt" {
			s.D.ForgetLimit(id) // stopped by you: it does not go on by itself after the limit either
		}
		err = controlChat(c, action, r)
	default:
		http.Error(w, "not for a chat without a task", http.StatusBadRequest)
		return
	}
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// readPlan: the answer to the agent's plan, of a task or of a chat: the body {do: approve | revise | reject, mode,
// model, notes: {steps: [{n, title, note}], whole}}; do may come in the query instead (a notification's button:
// ?do=approve, no body). A body that is not such JSON is an error: nothing is done with it.
func readPlan(r *http.Request) (string, dispatch.PlanApproval, error) {
	var in struct {
		Do, Mode, Model string
		Notes           chat.PlanNotes
	}
	if err := decodeJSON(r, 200<<10, &in); err != nil {
		return "", dispatch.PlanApproval{}, err
	}
	if in.Do == "" {
		in.Do = r.URL.Query().Get("do")
	}
	return in.Do, dispatch.PlanApproval{Mode: in.Mode, Model: in.Model, Notes: in.Notes}, nil
}

// decodeJSON reads the body's JSON (at most limit bytes) into v; an empty body leaves v as it is. The error says
// the body could not be read, for {ok:false, error}.
func decodeJSON(r *http.Request, limit int64, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(v); err != nil && err != io.EOF {
		return fmt.Errorf("could not read the request: %w", err)
	}
	return nil
}

// controlChat: the running agent's controls in its chat — an answer to its question, an option of one, the mode,
// a tool permission, or stopping the current step (interrupt).
func controlChat(c *chat.Session, action string, r *http.Request) error {
	if c == nil {
		return fmt.Errorf("the agent is not running")
	}
	q := r.URL.Query()
	switch action {
	case "answer":
		var answers map[string]string
		if err := decodeJSON(r, 200<<10, &answers); err != nil {
			return err
		}
		return c.Answer(q.Get("req"), answers)
	case "choose":
		// An option of the agent's question, from a notification's button.
		option, err := strconv.Atoi(q.Get("option"))
		if err != nil {
			return err
		}
		return c.Choose(q.Get("req"), option)
	case "mode":
		return c.SetMode(q.Get("to"))
	case "permit":
		return c.Permit(q.Get("req"), q.Get("decision"))
	default:
		return c.Interrupt()
	}
}
