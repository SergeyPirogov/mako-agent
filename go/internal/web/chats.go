package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
)

// chatRoutes: the chats without a task — their list, opening one with its first message, and removing one. The
// conversation itself is /api/chat?id=<the chat's id>; the messages, answers and the rest go to /api/{action}, as a
// task's do (freeChatAction).
func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/chats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"chats": s.D.FreeChats()})
	})
	// A new chat: {project, text}.
	mux.HandleFunc("POST /api/chats/new", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct{ Project, Text string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 200<<10)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
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
		if err := s.D.RemoveFreeChat(r.URL.Query().Get("id")); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
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
		if c != nil {
			c.Stop()
		}
	case "plan":
		// Not the task's ApprovePlan: it would note the plan in the task store, making a task of the chat.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 200<<10))
		err = s.D.FreePlan(id, r.URL.Query().Get("req"), r.URL.Query().Get("do") == "approve", string(body))
	case "permit", "answer", "choose", "interrupt", "mode":
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
		json.NewDecoder(io.LimitReader(r.Body, 200<<10)).Decode(&answers)
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
