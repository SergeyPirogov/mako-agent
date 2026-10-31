package gitx

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The branch's history as Orca draws it: the commits with their parents (for the graph) and the branches and tags
// that point at them.

type LogCommit struct {
	Hash    string   `json:"hash"`
	Parents []string `json:"parents"`
	Refs    []string `json:"refs,omitempty"` // "HEAD -> fix/32-…", "origin/master", "tag: v1.2"
	Subject string   `json:"subject"`
	Author  string   `json:"author"`
	At      int64    `json:"at"` // unix seconds
	Mine    bool     `json:"mine,omitempty"`
}

type History struct {
	Commits []LogCommit `json:"commits"`
	More    bool        `json:"more"` // there are older ones
}

// Log: the last n commits reachable from HEAD, newest first, in an order the graph can draw; mine marks the
// branch's own (not on origin/<base>).
func Log(ctx context.Context, worktree, baseBranch string, n int) (*History, error) {
	out, err := Run(ctx, worktree, "log", "--topo-order", "-n", strconv.Itoa(n+1), "--format=%H%x1f%P%x1f%D%x1f%s%x1f%an%x1f%ct", "HEAD")
	if err != nil {
		return nil, err
	}
	mine := map[string]bool{}
	base := "origin/" + baseBranch
	if _, err := Run(ctx, worktree, "rev-parse", "--verify", "-q", base); err != nil {
		base = baseBranch
	}
	if own, err := Run(ctx, worktree, "rev-list", base+"..HEAD"); err == nil {
		for _, h := range lines(own) {
			mine[h] = true
		}
	}
	h := &History{Commits: []LogCommit{}}
	for _, l := range lines(out) {
		f := strings.Split(l, "\x1f")
		if len(f) < 6 {
			continue
		}
		if len(h.Commits) == n {
			h.More = true
			break
		}
		at, _ := strconv.ParseInt(f[5], 10, 64)
		c := LogCommit{Hash: f[0], Parents: strings.Fields(f[1]), Subject: f[3], Author: f[4], At: at, Mine: mine[f[0]]}
		for _, r := range strings.Split(f[2], ", ") {
			if r = strings.TrimSpace(r); r != "" && r != "origin/HEAD" {
				c.Refs = append(c.Refs, r)
			}
		}
		h.Commits = append(h.Commits, c)
	}
	return h, nil
}

var fullHash = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// CommitDetail: a commit's whole message and the files it changed.
type CommitDetail struct {
	Message string        `json:"message"`
	Files   []ChangedFile `json:"files"`
}

func CommitInfo(ctx context.Context, worktree, hash string) (*CommitDetail, error) {
	if !fullHash.MatchString(hash) {
		return nil, fmt.Errorf("немає такого коміту")
	}
	msg, err := Run(ctx, worktree, "show", "-s", "--format=%B", hash)
	if err != nil {
		return nil, err
	}
	d := &CommitDetail{Message: msg, Files: []ChangedFile{}}
	stat, _ := Run(ctx, worktree, "-c", "core.quotePath=false", "show", "--format=", "--numstat", "-M", hash)
	stats := map[string][2]int{}
	for _, l := range lines(stat) {
		if f := strings.SplitN(l, "\t", 3); len(f) == 3 {
			add, _ := strconv.Atoi(f[0])
			del, _ := strconv.Atoi(f[1])
			stats[renamedTo(f[2])] = [2]int{add, del}
		}
	}
	names, _ := Run(ctx, worktree, "-c", "core.quotePath=false", "show", "--format=", "--name-status", "-M", hash)
	for _, l := range lines(names) {
		f := strings.Split(l, "\t")
		if len(f) < 2 {
			continue
		}
		p := f[len(f)-1]
		d.Files = append(d.Files, ChangedFile{Path: p, Status: f[0][:1], Additions: stats[p][0], Deletions: stats[p][1]})
	}
	return d, nil
}
