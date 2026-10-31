package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// What a task's branch changed against its base branch, committed or not: the board shows it next to the agent.

type ChangedFile struct {
	Path string `json:"path"`
	// "A" added, "M" modified, "D" deleted, "R" renamed, "?" new and not committed yet
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type Commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

type ChangeSet struct {
	Base    string        `json:"base"`
	Commits []Commit      `json:"commits"`
	Files   []ChangedFile `json:"files"`
}

const (
	maxDiff    = 400 << 10
	maxNewFile = 200 << 10
)

// forkPoint: where the branch left the base branch, so its diff is the task's own work.
func forkPoint(ctx context.Context, worktree, baseBranch string) string {
	for _, ref := range []string{"origin/" + baseBranch, baseBranch} {
		if out, err := Run(ctx, worktree, "merge-base", "HEAD", ref); err == nil && out != "" {
			return out
		}
	}
	return "HEAD"
}

// Changes: the branch's commits and files against baseBranch, uncommitted work included.
func Changes(ctx context.Context, worktree, baseBranch string) (*ChangeSet, error) {
	base := forkPoint(ctx, worktree, baseBranch)
	cs := &ChangeSet{Base: baseBranch, Commits: []Commit{}, Files: []ChangedFile{}}
	log, err := Run(ctx, worktree, "log", "--format=%h%x09%s", base+"..HEAD")
	if err != nil {
		return nil, err
	}
	for _, l := range lines(log) {
		hash, subject, _ := strings.Cut(l, "\t")
		cs.Commits = append(cs.Commits, Commit{Hash: hash, Subject: subject})
	}
	numstat, err := Run(ctx, worktree, "-c", "core.quotePath=false", "diff", "--numstat", "-M", base)
	if err != nil {
		return nil, err
	}
	stats := map[string][2]int{}
	for _, l := range lines(numstat) {
		f := strings.SplitN(l, "\t", 3)
		if len(f) == 3 {
			add, _ := strconv.Atoi(f[0])
			del, _ := strconv.Atoi(f[1])
			stats[renamedTo(f[2])] = [2]int{add, del}
		}
	}
	names, err := Run(ctx, worktree, "-c", "core.quotePath=false", "diff", "--name-status", "-M", base)
	if err != nil {
		return nil, err
	}
	for _, l := range lines(names) {
		f := strings.Split(l, "\t")
		if len(f) < 2 {
			continue
		}
		p := f[len(f)-1]
		s := stats[p]
		cs.Files = append(cs.Files, ChangedFile{Path: p, Status: f[0][:1], Additions: s[0], Deletions: s[1]})
	}
	untracked, err := Run(ctx, worktree, "-c", "core.quotePath=false", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, p := range lines(untracked) {
		n := 0
		if b, err := os.ReadFile(filepath.Join(worktree, p)); err == nil && len(b) <= maxNewFile && !bytes.Contains(b, []byte{0}) {
			n = len(lines(string(b)))
		}
		cs.Files = append(cs.Files, ChangedFile{Path: p, Status: "?", Additions: n})
	}
	return cs, nil
}

// FileDiff: one file's diff against the fork point; a new file not committed yet shows as added.
func FileDiff(ctx context.Context, worktree, baseBranch, path string) (string, error) {
	if path == "" || strings.HasPrefix(path, ":") || strings.HasPrefix(path, "-") {
		return "", fmt.Errorf("немає такого файла")
	}
	// Only a file git lists as untracked is read from disk: nothing outside the worktree.
	if out, _ := Run(ctx, worktree, "-c", "core.quotePath=false", "ls-files", "--others", "--exclude-standard", "--", path); out == path {
		b, err := os.ReadFile(filepath.Join(worktree, path))
		if err != nil {
			return "", err
		}
		if len(b) > maxNewFile || bytes.Contains(b, []byte{0}) {
			return "Binary or large file " + path + " (not committed yet)", nil
		}
		ls := lines(strings.TrimSuffix(string(b), "\n"))
		var sb strings.Builder
		fmt.Fprintf(&sb, "new file, not committed yet\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(ls))
		for _, l := range ls {
			sb.WriteString("+" + l + "\n")
		}
		return sb.String(), nil
	}
	diff, err := Run(ctx, worktree, "diff", "-M", "--no-color", forkPoint(ctx, worktree, baseBranch), "--", path)
	if err != nil {
		return "", err
	}
	if len(diff) > maxDiff {
		diff = diff[:maxDiff] + "\n… (diff cut: too long)"
	}
	return diff, nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// renamedTo: numstat writes a rename as "dir/{old => new}.go" or "old => new"; the new path.
func renamedTo(p string) string {
	if i, j := strings.Index(p, "{"), strings.Index(p, "}"); i >= 0 && j > i {
		if _, to, ok := strings.Cut(p[i+1:j], " => "); ok {
			return strings.ReplaceAll(p[:i]+to+p[j+1:], "//", "/")
		}
	}
	if _, to, ok := strings.Cut(p, " => "); ok {
		return to
	}
	return p
}
