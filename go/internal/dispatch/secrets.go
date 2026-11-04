package dispatch

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
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

// The command check above sees only what the Bash command says; eval, a git alias or a script that pushes get
// past it. So the task's worktree has a pre-push hook of Mako's too (gitx.InstallHooks): git asks Mako before
// any push from there, the agent's, a person's or Mako's own, and the push is stopped when Mako finds secrets,
// cannot check, or does not answer.

// gitPush: git push (git -C dir push, git -c k=v push too).
const gitPush = `\bgit(\s+(-C|-c)\s+\S+|\s+--?[\w-]+(=\S+)?)*\s+push\b`

// pushCommand: git push, gh pr create, glab mr create.
var pushCommand = regexp.MustCompile(gitPush + `|\bgh\s+pr\s+create\b|\bglab\s+mr\s+create\b`)

var (
	gitPushCommand = regexp.MustCompile(gitPush)
	commandParts   = regexp.MustCompile(`&&|\|\||[;|\n]`)
	skipsHooks     = regexp.MustCompile(`(?i)(^|\s)--no-verify\b|core\.hooksPath`)
)

// skipsPushHook: the command pushes with the hooks turned off (--no-verify) or pointed elsewhere
// (-c core.hooksPath=…), which takes Mako's pre-push check with them. Each command of a line is read on its own:
// `git commit --no-verify && git push` skips only the commit's hooks.
func skipsPushHook(command string) bool {
	for _, part := range commandParts.Split(command, -1) {
		if gitPushCommand.MatchString(part) && skipsHooks.MatchString(part) {
			return true
		}
	}
	return false
}

// hooksSkipped: what the agent is told when it asks to push without the check.
const hooksSkipped = "Mako stopped the push: it skips the git hooks (--no-verify or core.hooksPath), and with them the check of the changes for secrets. " +
	"Push without those, so that the hooks run; if a hook of the repository fails, fix the cause."

// GuardURL: where the task's agent asks before a Bash command; the token is new for each start of the agent.
func (d *Dispatcher) GuardURL(id string) string {
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
	if skipsPushHook(e.ToolInput.Command) {
		d.Log(id, "push stopped: it skips the git hooks")
		return hookDecision("deny", hooksSkipped)
	}
	found, err := d.CheckSecrets(ctx, id)
	if err != nil {
		d.Log(id, "the secrets check failed, push stopped: "+err.Error())
		return hookDecision("deny", checkFailed(err))
	}
	if len(found) > 0 {
		return hookDecision("deny", secretsRequest(found))
	}
	// Unattended (AutoTake, AutoFix): what a task's text or an MR's comment talked the agent into stays on this
	// machine until you let it out.
	if unattended(d.Config()) {
		return hookDecision("ask", unattendedPush)
	}
	return nil
}

// unattended: tasks go to work (AutoTake) or MRs get fixed (AutoFix) with nobody looking.
func unattended(cfg *config.Config) bool { return cfg.Notifications.AutoTake || cfg.Terminal.AutoFix }

// orcaFor: the Orca settings a task's agent in Orca starts with. With AutoTake it only commits and you open the MR
// from Orca (MRBy "me"): Orca starts Claude with its own settings, so Mako's guard cannot ask you before it pushes.
func orcaFor(cfg *config.Config) config.Orca {
	o := cfg.Orca
	if cfg.Notifications.AutoTake {
		o.MRBy = "me"
	}
	return o
}

// mrBy: who opens the task's MR, "agent" or "me" (see orcaFor).
func (d *Dispatcher) mrBy(r core.Record) string {
	if onTerminal(r) {
		return d.Config().Orca.MRBy
	}
	return orcaFor(d.Config()).MRBy
}

// unattendedPush: why the guard asks you about a push or a new MR while tasks go to work by themselves.
const unattendedPush = "Mako asks a human to confirm pushes and new merge requests while tasks are taken (AutoTake) or MRs are fixed (AutoFix) automatically."

// hookDecision: Claude Code's PreToolUse hook output with the decision ("deny", "ask") and its reason.
// HookDeny: the hook's output that stops the command with the reason.
func HookDeny(reason string) map[string]any { return hookDecision("deny", reason) }

func hookDecision(decision, reason string) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       decision,
		"permissionDecisionReason": reason,
	}}
}

// checkFailed: what the agent is told when the push could not be checked: it is not let through unchecked.
func checkFailed(err error) string {
	return "Mako stopped the push: it could not check the branch changes for secrets (" + err.Error() + "). " +
		"Make sure the worktree is in order (no rebase or merge in progress) and try to push again."
}

// hookDir: the folder of the task's worktree hooks, next to its agent settings.
func (d *Dispatcher) hookDir(id string) string {
	return filepath.Join(d.Sessions.Dir, "hooks", unsafeChars.ReplaceAllString(id, "_"))
}

var unsafeChars = regexp.MustCompile(`[^\w.-]`)

// pushHooks: the worktrees get the pre-push hook; off in the tests whose board answers nobody.
var pushHooks = true

// installPushHook gives the task's worktree Mako's pre-push hook. Its token stays the same across agent starts
// and Mako's restarts: a push from the worktree is checked whenever Mako runs.
func (d *Dispatcher) installPushHook(ctx context.Context, id, worktree string) error {
	if !pushHooks || d.BoardURL == "" || worktree == "" {
		return nil
	}
	dir := d.hookDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	token := d.pushToken(id)
	if token == "" {
		token = randomToken()
		if err := os.WriteFile(filepath.Join(dir, ".token"), []byte(token), 0o600); err != nil {
			return err
		}
	}
	url := fmt.Sprintf("%s/prepush?id=%s&token=%s", d.BoardURL, escapeQuery(id), token)
	if err := gitx.WriteHooks(dir, url); err != nil {
		return err
	}
	return gitx.InstallHooks(ctx, worktree, dir)
}

// pushToken: the token of the task's pre-push hook, "" when it has none.
func (d *Dispatcher) pushToken(id string) string {
	b, err := os.ReadFile(filepath.Join(d.hookDir(id), ".token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// maxPushRefs: how much of git's list of refs a pre-push check reads; a push of more refs than this is stopped.
const maxPushRefs = 1 << 20

// PrePush answers the worktree's pre-push hook: 204 lets the push go, anything else stops it with the reason
// for whoever pushed. refs is what git gives the hook on stdin, "<local ref> <local sha> <remote ref> <remote
// sha>" per line: what those refs carry is what is read, whichever branch is checked out. A check that fails
// stops the push too.
func (d *Dispatcher) PrePush(ctx context.Context, id, token, refs string) (int, string) {
	want := d.pushToken(id)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(token)) != 1 {
		return http.StatusForbidden, "Mako stopped the push: the worktree's hook is not known to Mako, so the changes were not checked for secrets."
	}
	fail := func(err error) (int, string) {
		d.Log(id, "the secrets check failed, push stopped: "+err.Error())
		return http.StatusConflict, checkFailed(err)
	}
	if len(refs) > maxPushRefs {
		return fail(errors.New("too many refs in one push"))
	}
	wt, base, err := d.workOf(id)
	if err != nil {
		return fail(err)
	}
	diff, err := refsDiff(ctx, wt, base, refs)
	if err != nil {
		return fail(err)
	}
	// The task's own branch pushed: a clean result is the card's too.
	head, _ := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	whole := head != "" && slices.ContainsFunc(strings.Split(refs, "\n"), func(l string) bool {
		f := strings.Fields(l)
		return len(f) == 4 && f[1] == head
	})
	if found := d.scanDiff(id, diff, whole); len(found) > 0 {
		return http.StatusConflict, secretsRequest(found)
	}
	return http.StatusNoContent, ""
}

// pushDiff: what a push would carry; the tests make it fail.
var pushDiff = gitx.PushDiff

// refsDiff: what the refs of a push carry; the tests make it fail.
var refsDiff = gitx.RefsDiff

// CheckSecrets scans what a push of the task's branch would carry and keeps the result on the card: the
// findings, or nothing once the changes are clean. A new set of findings is logged and notified.
func (d *Dispatcher) CheckSecrets(ctx context.Context, id string) ([]secrets.Finding, error) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return nil, err
	}
	diff, err := pushDiff(ctx, wt, base)
	if err != nil {
		return nil, err
	}
	return d.scanDiff(id, diff, true), nil
}

// scanDiff reads diff for secrets and keeps the result on the card. whole: the diff is all that the task's
// branch would push, so a clean one clears the card; otherwise (another ref was pushed) findings are kept on
// the card but a clean result leaves it as it is.
func (d *Dispatcher) scanDiff(id, diff string, whole bool) []secrets.Finding {
	r, _ := d.Store.Get(id)
	allowed := map[string]bool{}
	for _, k := range r.SecretsAllowed {
		allowed[k] = true
	}
	found := secrets.Scan(diff, allowed)
	before := r.Secrets
	if len(found) == 0 {
		if !whole {
			return nil
		}
		d.Store.Update(id, func(rec *core.Record) { rec.Secrets = nil })
		if len(before) > 0 {
			notify.Withdraw(secretsTag(id))
			d.Log(id, "no secrets left in the changes, push let through")
		}
		return nil
	}
	d.Store.Update(id, func(rec *core.Record) { rec.Secrets = found })
	if !sameFindings(before, found) {
		d.Log(id, fmt.Sprintf("push stopped: %d %s in the changes look like secrets", len(found), plural(len(found), "finding", "findings", "findings")))
		notify.Show(notify.Notice{Kind: notify.Secrets, Tag: secretsTag(id), URL: d.BoardURL + "/#" + id,
			Title: i18n.T("notify.secrets", id), Message: findingsText(found, 3)})
	}
	return found
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
