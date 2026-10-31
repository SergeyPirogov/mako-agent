package orca

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

// Terminal is one of a workspace's terminals, as `orca terminal list` reports it.
type Terminal struct {
	Handle       string `json:"handle"`
	Title        string `json:"title"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	Connected    bool   `json:"connected"`
	LastOutputAt int64  `json:"lastOutputAt"` // ms
	Preview      string `json:"preview"`
	Agent        string `json:"agentIdentity"` // "claude", "codex", …; "" when Orca does not know
	ExitCause    any    `json:"exitCause"`
}

// Wait: what an agent waits for, when Orca sees it waiting (a permission prompt, a question).
type Wait struct {
	Source string `json:"source"` // "hook", "prompt-text", "title"
	Reason string `json:"reason"`
	Since  int64  `json:"since"`
}

// cli runs an Orca command with --json and decodes its result ({ok, result} or {ok:false, error}).
func cli(ctx context.Context, o config.Orca, dir string, out any, args ...string) error {
	command, err := Resolve(o.Command)
	if err != nil {
		return err
	}
	reply, ok := run(ctx, command, dir, append(args, "--json")...)
	if !ok {
		return fmt.Errorf("orca %s: %s", strings.Join(args[:2], " "), parseFailure(reply).Message)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	text := strings.TrimSpace(reply)
	if i := strings.Index(text, "{"); i > 0 {
		text = text[i:]
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return fmt.Errorf("orca %s: незрозуміла відповідь: %v", strings.Join(args[:2], " "), err)
	}
	return json.Unmarshal(env.Result, out)
}

// Terminals of the task's workspace: asked by its path; when Orca knows the workspace by another spelling of
// the path (a symlink like /var → /private/var) or Mako has none, every terminal is listed and those of the
// workspace picked by path or branch. all is how many terminals Orca has in total, for the card's explanation.
func Terminals(ctx context.Context, o config.Orca, worktree, branch string) (mine []Terminal, all int, err error) {
	var res struct {
		Terminals []Terminal `json:"terminals"`
	}
	dir := worktree
	if _, statErr := os.Stat(dir); statErr != nil {
		dir = ""
	}
	if worktree != "" {
		if err := cli(ctx, o, dir, &res, "terminal", "list", "--worktree", "path:"+worktree); err == nil && len(res.Terminals) > 0 {
			return res.Terminals, len(res.Terminals), nil
		}
	}
	res.Terminals = nil
	if err := cli(ctx, o, dir, &res, "terminal", "list"); err != nil {
		return nil, 0, err
	}
	for _, t := range res.Terminals {
		if (worktree != "" && samePath(t.WorktreePath, worktree)) || (branch != "" && strings.TrimPrefix(t.Branch, "refs/heads/") == branch) {
			mine = append(mine, t)
		}
	}
	return mine, len(res.Terminals), nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	clean := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return strings.ToLower(filepath.Clean(p))
	}
	return clean(a) == clean(b)
}

// AgentTerminal: the terminal Orca says runs an agent, else the first one (the agent's, for a Mako hand-off).
func AgentTerminal(ts []Terminal) *Terminal {
	for i := range ts {
		if ts[i].Agent != "" && ts[i].ExitCause == nil {
			return &ts[i]
		}
	}
	if len(ts) > 0 {
		return &ts[0]
	}
	return nil
}

// Waiting: what the agent in a terminal waits for; nil when it is not waiting (or Orca cannot tell).
func Waiting(ctx context.Context, o config.Orca, worktree, handle string) (*Wait, error) {
	var res struct {
		Terminal struct {
			AgentWait *Wait `json:"agentWait"`
		} `json:"terminal"`
	}
	err := cli(ctx, o, worktree, &res, "terminal", "show", "--terminal", handle)
	return res.Terminal.AgentWait, err
}

// Screen: what the terminal shows now, line by line.
func Screen(ctx context.Context, o config.Orca, worktree, handle string) ([]string, error) {
	var res struct {
		Terminal struct {
			Tail   []string `json:"tail"`
			Source string   `json:"source"`
		} `json:"terminal"`
	}
	err := cli(ctx, o, worktree, &res, "terminal", "read", "--terminal", handle, "--screen")
	if err == nil && res.Terminal.Source == "screen-unavailable" {
		return Tail(ctx, o, worktree, handle, 40)
	}
	return res.Terminal.Tail, err
}

// Tail: the last lines of the terminal's output stream.
func Tail(ctx context.Context, o config.Orca, worktree, handle string, lines int) ([]string, error) {
	var res struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	err := cli(ctx, o, worktree, &res, "terminal", "read", "--terminal", handle, "--limit", strconv.Itoa(lines))
	return res.Terminal.Tail, err
}

// Focus brings the terminal forward in Orca.
func Focus(ctx context.Context, o config.Orca, worktree, handle string) error {
	var res any
	return cli(ctx, o, worktree, &res, "terminal", "switch", "--terminal", handle)
}
