package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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
		body, ok := readBody(w, r, 200<<10)
		if !ok {
			return
		}
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
		err = s.controlChat(c, action, r)
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
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return fmt.Errorf("could not read the request: %w", err)
	}
	if int64(len(body)) > limit {
		return errors.New(errTooLong)
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(v); err != nil && err != io.EOF {
		return fmt.Errorf("could not read the request: %w", err)
	}
	return nil
}

// errTooLong is the answer to a body over its route's limit: it is refused, never cut and passed on.
const errTooLong = "too long"

// readBody reads the request's body, at most max bytes. A longer one is not cut: it answers {ok:false, error:"too
// long"} and reports false, so the caller just returns.
func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	switch {
	case err != nil:
		writeJSON(w, map[string]any{"ok": false, "error": "could not read the request: " + err.Error()})
	case int64(len(body)) > max:
		writeJSON(w, map[string]any{"ok": false, "error": errTooLong})
	default:
		return body, true
	}
	return nil, false
}

// chosenRules: the rules {"rules": [...]} a permission request's «Always allow selected» sends, nil when it sends
// none (a notification's button, an older board). Each must be a rule Mako may keep.
func chosenRules(r *http.Request) ([]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10+1))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	if len(body) > 16<<10 {
		return nil, errors.New(errTooLong)
	}
	var in struct {
		Rules *[]string `json:"rules"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("could not read the rules: %w", err)
	}
	if in.Rules == nil {
		return nil, nil
	}
	out := []string{}
	for _, x := range *in.Rules {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if !chat.ValidRule(x) {
			return nil, fmt.Errorf("%q is not a rule Mako can keep: name the command, e.g. Bash(git push:*)", x)
		}
		out = append(out, x)
	}
	if len(out) == 0 {
		return nil, errors.New("no rule is ticked: tick one, or press Allow")
	}
	return out, nil
}

// controlChat: the running agent's controls in its chat — an answer to its question, an option of one, the mode,
// a tool permission, or stopping the current step (interrupt).
func (s *Server) controlChat(c *chat.Session, action string, r *http.Request) error {
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
		req, decision := q.Get("req"), q.Get("decision")
		rules := c.AlwaysRules(req)
		var chosen []string // the rules ticked (and maybe edited) on the request; nil: all it suggests
		if decision == "always" {
			var err error
			if chosen, err = chosenRules(r); err != nil {
				return err
			}
			if chosen != nil {
				rules = chosen
			}
		}
		if err := c.Permit(req, decision, chosen); err != nil {
			return err
		}
		rules = slices.DeleteFunc(slices.Clone(rules), func(x string) bool { return !chat.ValidRule(x) })
		// «Always» is for every agent after this one too: their Claude starts with the rules in --allowedTools.
		if decision == "always" && len(rules) > 0 {
			if err := s.allowTools(r.Context(), rules); err != nil {
				return fmt.Errorf("allowed for this agent, but not saved for the next ones: %w", err)
			}
		}
		return nil
	default:
		return c.Interrupt()
	}
}
