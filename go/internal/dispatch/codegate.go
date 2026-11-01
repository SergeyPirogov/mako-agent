package dispatch

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// codeGate: what the agent of a question may not do, decided at once: the tools that edit files, the plan mode, and
// the shell commands that change the repository or delete files. The worktree is shared with the clone (refs,
// .git), so a git push, a commit or a branch deleted there is the clone's too. Anything else (reading, searching,
// tests, builds, git log) goes through without asking. It is a guard against the obvious, not a sandbox: a
// determined command can still write somewhere; the worktree's warning shows what changed in it.
func codeGate(tool string, input json.RawMessage) string {
	switch {
	case slices.Contains(CodeDeny, tool):
		return "тут не можна змінювати код; читай, шукай і запускай команди, але файлів не змінюй."
	case tool == "ExitPlanMode" || tool == "EnterPlanMode":
		return "тут немає режиму плану: просто відповідай."
	case tool == "Bash":
		var in struct {
			Command string `json:"command"`
		}
		json.Unmarshal(input, &in)
		if why := refusedCommand(in.Command); why != "" {
			return why + " Тут можна читати, шукати й запускати тести чи збірку, але не змінювати репозиторій і файли."
		}
	}
	return ""
}

// gitWrites: the git commands that change the repository, its refs or the files of the worktree.
var gitWrites = []string{"push", "commit", "reset", "checkout", "switch", "restore", "branch", "tag", "rebase", "merge",
	"pull", "stash", "cherry-pick", "revert", "am", "apply", "clean", "rm", "mv", "worktree", "update-ref", "gc",
	"prune", "remote", "config", "filter-branch", "replace", "notes", "submodule", "init", "clone"}

// fileWrites: the commands that delete, move or change files or their owner.
var fileWrites = []string{"rm", "rmdir", "mv", "unlink", "shred", "truncate", "dd", "chmod", "chown", "ln", "sudo", "doas"}

// shellParts splits a command line at ; & | newlines and the substitutions $( and `, roughly as the shell runs them.
var shellParts = regexp.MustCompile("[;&|\\n`]|\\$\\(")

// refusedCommand: why the command is not run here, "" when it may be.
func refusedCommand(cmd string) string {
	for _, part := range shellParts.Split(cmd, -1) {
		words := strings.Fields(strings.NewReplacer("(", " ", ")", " ", "{", " ", "}", " ").Replace(part))
		// Variables and wrappers before the command itself.
		for len(words) > 0 && (strings.Contains(words[0], "=") || slices.Contains([]string{"env", "command", "exec", "nohup", "time", "xargs", "then", "do", "else", "!"}, words[0])) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		name := filepath.Base(strings.Trim(words[0], `"'`))
		if slices.Contains(fileWrites, name) {
			return "Mako не запускає тут " + name + "."
		}
		if name == "find" && slices.ContainsFunc(words, func(w string) bool { return w == "-delete" || w == "-exec" || w == "-execdir" }) {
			return "Mako не запускає тут find з -delete чи -exec."
		}
		if name != "git" {
			continue
		}
		// git's own options before its command: -C dir, -c key=value, --git-dir=…
		args := words[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			if (args[0] == "-C" || args[0] == "-c") && len(args) > 1 {
				args = args[1:]
			}
			args = args[1:]
		}
		if len(args) > 0 && slices.Contains(gitWrites, args[0]) {
			if args[0] == "branch" || args[0] == "tag" || args[0] == "remote" || args[0] == "stash" || args[0] == "worktree" || args[0] == "config" {
				if readOnlyListing(args) {
					continue
				}
			}
			return "Mako не запускає тут git " + args[0] + ": worktree ділить гілки й .git з клоном."
		}
	}
	return ""
}

// readOnlyListing: git branch, tag, remote, stash, worktree or config only listing what there is.
func readOnlyListing(args []string) bool {
	rest := args[1:]
	switch args[0] {
	case "stash", "worktree":
		return len(rest) > 0 && rest[0] == "list"
	case "remote":
		return len(rest) == 0 || rest[0] == "-v" || rest[0] == "show" || rest[0] == "get-url"
	case "config":
		return len(rest) > 0 && (rest[0] == "--get" || rest[0] == "--list" || rest[0] == "-l" || rest[0] == "--get-all" || rest[0] == "get")
	}
	// branch, tag: listing unless it names something to make, delete or move.
	for _, a := range rest {
		switch {
		case a == "-d" || a == "-D" || a == "--delete" || a == "-m" || a == "-M" || a == "--move" || a == "-c" || a == "-C" || a == "--copy" ||
			a == "-f" || a == "--force" || a == "-u" || a == "--set-upstream-to" || a == "--unset-upstream" || a == "--edit-description" || a == "-a" && args[0] == "tag" || a == "-s":
			return false
		case !strings.HasPrefix(a, "-"):
			// A name with no listing option before it makes a branch or tag.
			if !slices.ContainsFunc(rest, func(o string) bool {
				return o == "-l" || o == "--list" || o == "--contains" || o == "--merged" || o == "--no-merged" || o == "--points-at" || o == "--format"
			}) {
				return false
			}
		}
	}
	return true
}
