package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/secrets"
)

// The push guard: before Mako's own agent runs a Bash command that pushes the branch or opens the MR, Claude
// Code's PreToolUse hook asks Mako. Mako reads what the push would carry and refuses it when something looks
// like a secret: the agent gets the list and the request to take them out, the card and a notification show
// where they are. «Це не секрет» on the card lets a line through.

// pushCommand: git push (git -C dir push, git -c k=v push too), gh pr create, glab mr create.
var pushCommand = regexp.MustCompile(`\bgit(\s+(-C|-c)\s+\S+|\s+--?[\w-]+(=\S+)?)*\s+push\b|\bgh\s+pr\s+create\b|\bglab\s+mr\s+create\b`)

// guardURL: where the task's agent asks before a Bash command; the token is new for each start of the agent.
func (d *Dispatcher) guardURL(id string) string {
	token := randomToken()
	d.mu.Lock()
	if d.guardTokens == nil {
		d.guardTokens = map[string]string{}
	}
	d.guardTokens[id] = token
	d.mu.Unlock()
	return fmt.Sprintf("%s/guard?id=%s&token=%s", d.BoardURL, escapeQuery(id), token)
}

// GuardToken: the token the task's agent asks the guard with, "" when it has none.
func (d *Dispatcher) GuardToken(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.guardTokens[id]
}

// GuardPush answers Claude Code's PreToolUse hook: the hook's output that refuses a push carrying secrets, nil
// to let the command run.
func (d *Dispatcher) GuardPush(ctx context.Context, id string, hook []byte) map[string]any {
	var e struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(hook, &e) != nil || e.ToolName != "Bash" || !pushCommand.MatchString(e.ToolInput.Command) {
		return nil
	}
	found, err := d.CheckSecrets(ctx, id)
	if err != nil {
		d.Log(id, "the secrets check failed, push not stopped: "+err.Error())
		return nil
	}
	if len(found) == 0 {
		return nil
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": secretsRequest(found),
	}}
}

// CheckSecrets scans what a push of the task's branch would carry and keeps the result on the card: the
// findings, or nothing once the changes are clean. A new set of findings is logged and notified.
func (d *Dispatcher) CheckSecrets(ctx context.Context, id string) ([]secrets.Finding, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return nil, err
	}
	diff, err := gitx.PushDiff(ctx, wt, base)
	if err != nil {
		return nil, err
	}
	r, _ := d.Store.Get(id)
	allowed := map[string]bool{}
	for _, k := range r.SecretsAllowed {
		allowed[k] = true
	}
	found := secrets.Scan(diff, allowed)
	before := r.Secrets
	d.Store.Update(id, func(rec *core.Record) { rec.Secrets = found })
	if len(found) == 0 {
		if len(before) > 0 {
			notify.Withdraw(secretsTag(id))
			d.Log(id, "no secrets left in the changes, push let through")
		}
		return nil, nil
	}
	if !sameFindings(before, found) {
		d.Log(id, fmt.Sprintf("push stopped: %d %s in the changes look like secrets", len(found), plural(len(found), "finding", "findings", "findings")))
		notify.Show(notify.Notice{Kind: notify.Secrets, Tag: secretsTag(id), URL: d.BoardURL + "/#" + id,
			Title: i18n.T("notify.secrets", id), Message: findingsText(found, 3)})
	}
	return found, nil
}

// AllowSecret: «Це не секрет» for a line; the card forgets its findings, and when none are left the agent hears
// it may push.
func (d *Dispatcher) AllowSecret(id, key string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return fmt.Errorf("no such task")
	}
	if key == "" || !slices.ContainsFunc(r.Secrets, func(f secrets.Finding) bool { return f.Key == key }) {
		return fmt.Errorf("no such finding")
	}
	var left []secrets.Finding
	d.Store.Update(id, func(rec *core.Record) {
		if !slices.Contains(rec.SecretsAllowed, key) {
			rec.SecretsAllowed = append(rec.SecretsAllowed, key)
		}
		rec.Secrets = slices.DeleteFunc(rec.Secrets, func(f secrets.Finding) bool { return f.Key == key })
		left = rec.Secrets
	})
	d.Log(id, "line marked as not a secret")
	if len(left) == 0 {
		notify.Withdraw(secretsTag(id))
		if d.Live(id) {
			d.sayTo(id, "The secrets-check findings were marked as \"not a secret\". You can push.")
		}
	}
	return nil
}

func secretsTag(id string) string { return "secrets-" + id }

// secretsRequest: what the agent is told when its push is stopped.
func secretsRequest(found []secrets.Finding) string {
	return "Mako stopped the push: the branch changes look like secrets:\n" + findingsText(found, 20) +
		"\n\nRemove them from the code: move them to environment variables or a config outside the repository, and put .env files in .gitignore. " +
		"If a secret is already committed, rewrite the branch history (git commit --amend, git rebase) so that it is in no commit, and try to push again. " +
		"If it is not a secret, say so: the person can allow the line on the task card (\"Not a secret\")."
}

// findingsText: "- app/config.go:12 — GitHub-токен (ghp_Ab…(40))", at most n of them.
func findingsText(found []secrets.Finding, n int) string {
	var b []string
	for i, f := range found {
		if i == n {
			b = append(b, i18n.T("notify.andMore", len(found)-n))
			break
		}
		at := f.File
		if f.Line > 0 {
			at += fmt.Sprintf(":%d", f.Line)
		}
		if f.Commit != "" {
			at += " (" + i18n.T("notify.commit") + " " + f.Commit + ")"
		}
		line := "- " + at + " — " + f.Kind
		if f.Masked != "" {
			line += " (" + f.Masked + ")"
		}
		b = append(b, line)
	}
	return strings.Join(b, "\n")
}

func sameFindings(a, b []secrets.Finding) bool {
	return slices.EqualFunc(a, b, func(x, y secrets.Finding) bool { return x.Key == y.Key && x.Kind == y.Kind })
}
