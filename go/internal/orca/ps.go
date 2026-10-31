package orca

import (
	"context"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

// Workspace is one row of `orca worktree ps`: the workspace and its agents, whether they run in a terminal or
// in Orca's chat view.
type Workspace struct {
	Path              string     `json:"path"`
	Branch            string     `json:"branch"`
	Status            string     `json:"status"`        // "active", "working", "permission", "done", "inactive"
	SidebarStatus     string     `json:"sidebarStatus"` // also "failed", "interrupted", "unconfirmed"
	LiveTerminalCount int        `json:"liveTerminalCount"`
	LastOutputAt      int64      `json:"lastOutputAt"`
	Preview           string     `json:"preview"`
	Agents            []AgentRow `json:"agents"`
}

// AgentRow is an agent in a workspace as Orca's sidebar knows it.
type AgentRow struct {
	PaneKey              string `json:"paneKey"`
	ParentPaneKey        string `json:"parentPaneKey"`
	State                string `json:"state"` // "working", "blocked", "waiting", "done"
	AgentType            string `json:"agentType"`
	LastAssistantMessage string `json:"lastAssistantMessage"`
	ToolName             string `json:"toolName"`
	ToolInput            string `json:"toolInput"`
	Interrupted          bool   `json:"interrupted"`
	AgentWait            *Wait  `json:"agentWait"`
	StateStartedAt       int64  `json:"stateStartedAt"`
	UpdatedAt            int64  `json:"updatedAt"`
	HasLivePty           bool   `json:"hasLivePty"`
}

// Workspaces: every workspace Orca has, with its agents and what they wait for.
func Workspaces(ctx context.Context, o config.Orca) ([]Workspace, error) {
	var res struct {
		Worktrees []Workspace `json:"worktrees"`
	}
	err := cli(ctx, o, "", &res, "worktree", "ps", "--agent-wait")
	return res.Worktrees, err
}

// FindWorkspace: the task's workspace by its path (any spelling of it) or, failing that, its branch.
func FindWorkspace(ws []Workspace, path, branch string) *Workspace {
	for i := range ws {
		if path != "" && samePath(ws[i].Path, path) {
			return &ws[i]
		}
	}
	for i := range ws {
		if branch != "" && strings.TrimPrefix(ws[i].Branch, "refs/heads/") == branch {
			return &ws[i]
		}
	}
	return nil
}

// MainAgent: the workspace's top agent (not a subagent), nil when it has none.
func (w *Workspace) MainAgent() *AgentRow {
	for i := range w.Agents {
		if w.Agents[i].ParentPaneKey == "" {
			return &w.Agents[i]
		}
	}
	if len(w.Agents) > 0 {
		return &w.Agents[0]
	}
	return nil
}

// Open brings the Orca app forward (starting it when it is closed).
func Open(ctx context.Context, o config.Orca) error {
	var res any
	return cli(ctx, o, "", &res, "open")
}
