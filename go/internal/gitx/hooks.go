package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The push guard of a task's worktree: git's own pre-push hook asks Mako before anything leaves, so a push is
// checked however it is started (a script, eval, an alias, Mako itself). The worktree's core.hooksPath points at
// a folder of Mako's; each hook there hands over to the repository's own hook of that name, so husky,
// pre-commit and the like keep working.

// clientHooks: the hooks git runs in a working copy that Mako's folder passes on to the repository's own.
var clientHooks = []string{"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge", "pre-push",
	"pre-auto-gc", "post-rewrite", "post-index-change", "push-to-checkout", "sendemail-validate"}

// hookScript: the hook itself, $1 the hook's name. pre-push asks url with what is being pushed and stops the
// push unless Mako answers 204: an error or no answer stops it too, with the reason on stderr.
const hookScript = `#!/bin/sh
# Mako: the secrets check before a push, then the repository's own hook.
name=$1
shift
# The hooks the worktree had before this one: the repository's setting, else the user's or the system's (shared
# hooks in ~/.git-hooks), else the repository's own folder. Not --worktree: that is this folder.
orig=
for scope in --local --global --system; do
	orig=$(git config "$scope" --type=path --get core.hooksPath 2>/dev/null)
	[ -n "$orig" ] && break
done
[ -n "$orig" ] || orig="$(git rev-parse --git-common-dir)/hooks"
own="$orig/$name"
here=$(cd "$(dirname "$0")" && pwd -P)
if [ -d "$orig" ] && [ "$(cd "$orig" && pwd -P)" = "$here" ]; then own=""; fi
if [ "$name" != pre-push ]; then
	if [ -n "$own" ] && [ -x "$own" ]; then exec "$own" "$@"; fi
	exit 0
fi
tmp=$(mktemp "${TMPDIR:-/tmp}/mako-pre-push.XXXXXX") || { echo "Mako stopped the push: no temporary file for the secrets check." >&2; exit 1; }
trap 'rm -f "$tmp" "$tmp.out"' EXIT
cat > "$tmp"
code=$(curl -s -m 60 -o "$tmp.out" -w '%{http_code}' -X POST -H 'Content-Type: text/plain' --data-binary @"$tmp" __URL__)
if [ "$code" != 204 ]; then
	if [ -s "$tmp.out" ]; then
		cat "$tmp.out" >&2
		echo >&2
	else
		echo "Mako stopped the push: Mako did not answer (is it running?), so the changes were not checked for secrets." >&2
	fi
	exit 1
fi
if [ -n "$own" ] && [ -x "$own" ]; then
	"$own" "$@" < "$tmp"
	exit $?
fi
exit 0
`

// WriteHooks fills dir with Mako's hooks; pre-push asks url.
func WriteHooks(dir, url string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	main := filepath.Join(dir, "mako-hook")
	if err := writeExec(main, strings.Replace(hookScript, "__URL__", shellQuote(url), 1)); err != nil {
		return err
	}
	for _, name := range clientHooks {
		if err := writeExec(filepath.Join(dir, name), "#!/bin/sh\nexec "+shellQuote(main)+" "+name+" \"$@\"\n"); err != nil {
			return err
		}
	}
	return nil
}

// InstallHooks points worktree's core.hooksPath at dir, for that worktree alone: the repository's other
// worktrees and its main checkout keep their hooks. That takes extensions.worktreeConfig in the repository.
func InstallHooks(ctx context.Context, worktree, dir string) error {
	if out, _ := Run(ctx, worktree, "config", "--get", "core.worktree"); out != "" {
		return errors.New("the repository sets core.worktree: a hook for one worktree would move it")
	}
	if out, _ := Run(ctx, worktree, "config", "--type=bool", "--get", "extensions.worktreeConfig"); out != "true" {
		if _, err := Run(ctx, worktree, "config", "extensions.worktreeConfig", "true"); err != nil {
			return err
		}
	}
	if out, _ := Run(ctx, worktree, "config", "--worktree", "--get", "core.hooksPath"); out == dir {
		return nil
	}
	_, err := Run(ctx, worktree, "config", "--worktree", "core.hooksPath", dir)
	return err
}

// writeExec writes an executable file whole: a hook running meanwhile reads the old one or the new.
func writeExec(path, body string) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
