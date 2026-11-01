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
		return "code cannot be changed here; read, search and run commands, but do not change files."
	case tool == "ExitPlanMode" || tool == "EnterPlanMode":
		return "there is no plan mode here: just answer."
	case tool == "Bash":
		var in struct {
			Command string `json:"command"`
		}
		json.Unmarshal(input, &in)
		if why := refusedCommand(in.Command); why != "" {
			return why + " Here you can read, search and run tests or a build, but not change the repository or files; " +
				"do not work around this with other commands."
		}
	}
	return ""
}

// gitWrites: the git commands that change the repository, its refs or the files of the worktree.
var gitWrites = []string{"push", "commit", "reset", "checkout", "switch", "restore", "branch", "tag", "rebase", "merge",
	"pull", "stash", "cherry-pick", "revert", "am", "apply", "clean", "rm", "mv", "worktree", "update-ref", "gc",
	"prune", "remote", "config", "filter-branch", "replace", "notes", "submodule", "init", "clone", "symbolic-ref",
	"update-index", "read-tree", "checkout-index", "commit-tree", "fast-import", "lfs"}

// fileWrites: the commands that write, delete or move files, or change their owner.
var fileWrites = []string{"rm", "rmdir", "mv", "cp", "install", "tee", "touch", "unlink", "shred", "truncate", "dd",
	"chmod", "chown", "ln", "patch", "rsync", "sudo", "doas", "su"}

// shells: the commands that run their -c argument (eval: all of them) as a command line of its own.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh", "fish"}

// wrappers: what may come before the command itself; timeout and nice take one argument of their own.
var wrappers = []string{"env", "command", "exec", "nohup", "time", "xargs", "then", "do", "else", "elif", "if", "while",
	"until", "!", "builtin", "nice", "timeout", "stdbuf"}

// substitution: $(…) and `…`, which run a command line of their own wherever they stand, quoted or not.
var substitution = regexp.MustCompile("\\$\\(([^()]*)\\)|`([^`]*)`")

// refusedCommand: why the command line is not run here, "" when it may be.
func refusedCommand(cmd string) string {
	return refusedLine(cmd, 0)
}

func refusedLine(cmd string, depth int) string {
	if depth > 5 {
		return "Mako does not parse such a nested command."
	}
	for _, m := range substitution.FindAllStringSubmatch(cmd, -1) {
		if why := refusedLine(m[1]+m[2], depth+1); why != "" {
			return why
		}
	}
	segments, writes := splitShell(substitution.ReplaceAllString(cmd, "x"))
	if len(writes) > 0 {
		return "Mako does not allow writing to files (" + writes[0] + ")."
	}
	for _, words := range segments {
		if why := refusedWords(words, depth); why != "" {
			return why
		}
	}
	return ""
}

func refusedWords(words []string, depth int) string {
	for len(words) > 0 && (isAssignment(words[0]) || slices.Contains(wrappers, words[0])) {
		if (words[0] == "timeout" || words[0] == "nice") && len(words) > 1 && !strings.HasPrefix(words[1], "-") {
			words = words[1:]
		}
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	name, args := filepath.Base(words[0]), words[1:]
	switch {
	case slices.Contains(fileWrites, name):
		return "Mako does not run " + name + " here."
	case name == "eval":
		return refusedLine(strings.Join(args, " "), depth+1)
	case slices.Contains(shells, name):
		for i, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
				return refusedLine(args[i+1], depth+1)
			}
		}
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			return "Mako does not run scripts through " + name + " here: run the script itself."
		}
	case (name == "sed" || name == "gsed" || name == "perl") && slices.ContainsFunc(args, func(a string) bool {
		return a == "--in-place" || strings.HasPrefix(a, "--in-place=") || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i")
	}):
		return "Mako does not allow changing files through " + name + " -i."
	case name == "find" && slices.ContainsFunc(args, func(a string) bool {
		return a == "-delete" || a == "-exec" || a == "-execdir" || a == "-ok" || a == "-fprint"
	}):
		return "Mako does not run find with -delete or -exec here."
	case name == "git":
		return refusedGit(args)
	}
	return ""
}

func refusedGit(args []string) string {
	// git's own options before its command: -C dir, -c key=value, --git-dir=…
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if (args[0] == "-C" || args[0] == "-c" || args[0] == "--git-dir" || args[0] == "--work-tree") && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return ""
	}
	sub := args[0]
	if sub == "fetch" {
		// A refspec with a destination (main:main, +HEAD:x) writes the clone's branches; a plain fetch only the
		// remote-tracking ones. The first word that is no option is the remote, a URL may have a colon.
		positional := 0
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if positional++; positional > 1 && strings.Contains(a, ":") {
				return "Mako does not run git fetch into local branches here (" + a + "): the worktree shares branches with the clone."
			}
		}
		return ""
	}
	if !slices.Contains(gitWrites, sub) || readOnlyListing(args) {
		return ""
	}
	return "Mako does not run git " + sub + " here: the worktree shares branches and .git with the clone."
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	return i > 0 && !strings.ContainsAny(w[:i], "-/.")
}

// splitShell splits a command line into its simple commands (at ; & | newlines and brackets outside quotes), each
// a list of words with the quotes taken off, and tells the files its redirections write to (not /dev/null).
func splitShell(cmd string) (segments [][]string, writes []string) {
	var words []string
	var word strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, word.String())
		}
		word.Reset()
		inWord = false
	}
	flushSegment := func() {
		flushWord()
		if len(words) > 0 {
			segments = append(segments, words)
		}
		words = nil
	}
	r := []rune(cmd)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\' && i+1 < len(r):
			i++
			word.WriteRune(r[i])
			inWord = true
		case c == '\'':
			inWord = true
			for i++; i < len(r) && r[i] != '\''; i++ {
				word.WriteRune(r[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(r) && r[i] != '"'; i++ {
				if r[i] == '\\' && i+1 < len(r) {
					i++
				}
				word.WriteRune(r[i])
			}
		case c == ' ' || c == '\t':
			flushWord()
		case strings.ContainsRune(";&|\n(){}", c):
			// 2>&1, &> and >&2 are redirections, not separators.
			if c == '&' && i+1 < len(r) && r[i+1] == '>' {
				continue
			}
			if c == '&' && i > 0 && r[i-1] == '>' {
				continue
			}
			flushSegment()
		case c == '>':
			// A file descriptor before it (2>) belongs to the redirection, not to the words.
			if s := word.String(); inWord && strings.Trim(s, "0123456789") == "" {
				word.Reset()
				inWord = false
			}
			flushWord()
			if i+1 < len(r) && (r[i+1] == '>' || r[i+1] == '|') {
				i++
			}
			if i+1 < len(r) && r[i+1] == '&' {
				// >&2: to another descriptor.
				for i++; i+1 < len(r) && (r[i+1] >= '0' && r[i+1] <= '9' || r[i+1] == '-'); i++ {
				}
				continue
			}
			for i+1 < len(r) && (r[i+1] == ' ' || r[i+1] == '\t') {
				i++
			}
			var target strings.Builder
			for i+1 < len(r) && !strings.ContainsRune(" \t;&|\n(){}<>", r[i+1]) {
				i++
				if r[i] != '"' && r[i] != '\'' {
					target.WriteRune(r[i])
				}
			}
			if t := target.String(); t != "/dev/null" && t != "/dev/stdout" && t != "/dev/stderr" && t != "/dev/tty" {
				writes = append(writes, "> "+t)
			}
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	flushSegment()
	return segments, writes
}

// readOnlyListing: git branch, tag, remote, stash, worktree or config only listing what there is.
func readOnlyListing(args []string) bool {
	rest := args[1:]
	switch args[0] {
	case "branch", "tag":
		// below
	case "stash", "worktree":
		return len(rest) > 0 && rest[0] == "list"
	case "remote":
		return len(rest) == 0 || rest[0] == "-v" || rest[0] == "show" || rest[0] == "get-url"
	case "config":
		return len(rest) > 0 && (rest[0] == "--get" || rest[0] == "--list" || rest[0] == "-l" || rest[0] == "--get-all" || rest[0] == "get")
	default:
		return false
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
