package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
)

// The journal of permission requests: what the agents asked you to allow, so Settings → Permissions shows what
// they ask most and a rule can be kept for it once, instead of answering each agent.

// permLogMax: how many requests the journal keeps, the newest.
const permLogMax = 500

// PermRequest: one request an agent made.
type PermRequest struct {
	At      int64    `json:"at"` // unix seconds
	Agent   string   `json:"agent"`
	Tool    string   `json:"tool"`
	Command string   `json:"command,omitempty"` // what it asked to run or touch, cut
	Rules   []string `json:"rules,omitempty"`   // the rules «Always allow» would keep
}

type permJournal struct {
	Requests []PermRequest `json:"requests"`
	// Dismissed: rule → when you said not to show it; it comes back with a request after that.
	Dismissed map[string]int64 `json:"dismissed,omitempty"`
}

// PermGroup: the requests one rule would have spared.
type PermGroup struct {
	Rule    string `json:"rule"`
	Count   int    `json:"count"`
	Agents  int    `json:"agents"`
	Last    int64  `json:"last"`
	Example string `json:"example,omitempty"`
}

// toolArgKeys: the input field that says what a tool asks for, as the board's permArg reads it.
var toolArgKeys = []string{"command", "file_path", "pattern", "url", "description", "prompt"}

// NotePermission keeps an agent's request in the journal.
func (d *Dispatcher) NotePermission(agent, tool string, input json.RawMessage, rules []string) {
	var in map[string]any
	json.Unmarshal(input, &in)
	cmd := ""
	for _, k := range toolArgKeys {
		if x, ok := in[k].(string); ok && x != "" {
			cmd = x
			break
		}
	}
	if r := []rune(cmd); len(r) > 300 {
		cmd = string(r[:299]) + "…"
	}
	d.permMu.Lock()
	defer d.permMu.Unlock()
	j := d.readPerms()
	j.Requests = append(j.Requests, PermRequest{At: time.Now().Unix(), Agent: agent, Tool: tool, Command: cmd, Rules: rules})
	if n := len(j.Requests); n > permLogMax {
		j.Requests = slices.Clone(j.Requests[n-permLogMax:])
	}
	d.writePerms(j)
}

// PermissionGroups: the requests by the rule that would have spared them, the most asked first; a rule already
// allowed, or dismissed with no request since, is left out. A request with no rule counts under its tool.
func (d *Dispatcher) PermissionGroups(allowed []string) []PermGroup {
	d.permMu.Lock()
	j := d.readPerms()
	d.permMu.Unlock()
	groups := map[string]*PermGroup{}
	agents := map[string]map[string]bool{}
	for _, r := range j.Requests {
		rules := r.Rules
		if len(rules) == 0 {
			rules = []string{r.Tool}
		}
		for _, rule := range rules {
			if slices.Contains(allowed, rule) {
				continue
			}
			g := groups[rule]
			if g == nil {
				g = &PermGroup{Rule: rule}
				groups[rule], agents[rule] = g, map[string]bool{}
			}
			g.Count++
			agents[rule][r.Agent] = true
			if r.At >= g.Last {
				g.Last, g.Example = r.At, r.Command
			}
		}
	}
	out := []PermGroup{}
	for rule, g := range groups {
		if at, ok := j.Dismissed[rule]; ok && g.Last <= at {
			continue
		}
		g.Agents = len(agents[rule])
		out = append(out, *g)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}
		if out[a].Last != out[b].Last {
			return out[a].Last > out[b].Last
		}
		return out[a].Rule < out[b].Rule
	})
	return out
}

// DismissPermissions hides the rules from the journal until an agent asks for them again.
func (d *Dispatcher) DismissPermissions(rules []string) {
	d.permMu.Lock()
	defer d.permMu.Unlock()
	j := d.readPerms()
	if j.Dismissed == nil {
		j.Dismissed = map[string]int64{}
	}
	now := time.Now().Unix()
	for _, r := range rules {
		if r = strings.TrimSpace(r); r != "" {
			j.Dismissed[r] = now
		}
	}
	d.writePerms(j)
}

func (d *Dispatcher) permsFile() string {
	if d.Config().StateDir == "" {
		return ""
	}
	return filepath.Join(d.Config().StateDir, "permissions.json")
}

// readPerms: the journal; an empty one when there is none yet or it cannot be read. Holds permMu.
func (d *Dispatcher) readPerms() permJournal {
	var j permJournal
	if f := d.permsFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			json.Unmarshal(b, &j)
		}
	}
	return j
}

// writePerms keeps the journal; it is only a help, so a failure to write it is not an error. Holds permMu.
func (d *Dispatcher) writePerms(j permJournal) {
	f := d.permsFile()
	if f == "" {
		return
	}
	b, _ := json.Marshal(j)
	if os.MkdirAll(filepath.Dir(f), 0o755) == nil {
		fsx.WriteFile(f, b, 0o600)
	}
}
