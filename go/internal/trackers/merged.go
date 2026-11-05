package trackers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// MergedRefs: the commits of an MR as its code host keeps them, so that a merged task can be done again from where
// it started (eval): Base, the base branch's commit the MR was compared with; Head, its last commit; Ref, the ref
// under which the host keeps that head after the branch is gone.
type MergedRefs struct {
	Base, Head, Ref string
}

// RefsHost is a code host that tells an MR's commits (GitLab, GitHub).
type RefsHost interface {
	MergedRefs(ctx context.Context, url string) (MergedRefs, error)
}

func (g *GitHub) MergedRefs(ctx context.Context, reviewURL string) (MergedRefs, error) {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return MergedRefs{}, fmt.Errorf("not a GitHub PR: %s", reviewURL)
	}
	var p struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := g.do(ctx, "GET", "/repos/"+m[1]+"/pulls/"+m[2], nil, &p); err != nil {
		return MergedRefs{}, err
	}
	if p.Base.SHA == "" || p.Head.SHA == "" {
		return MergedRefs{}, errors.New("GitHub did not tell the PR's commits")
	}
	return MergedRefs{Base: p.Base.SHA, Head: p.Head.SHA, Ref: "refs/pull/" + m[2] + "/head"}, nil
}

func (g *GitLab) MergedRefs(ctx context.Context, reviewURL string) (MergedRefs, error) {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return MergedRefs{}, fmt.Errorf("not a GitLab MR: %s", reviewURL)
	}
	var mr struct {
		IID      int `json:"iid"`
		DiffRefs *struct {
			Base string `json:"base_sha"`
			Head string `json:"head_sha"`
		} `json:"diff_refs"`
	}
	if err := g.do(ctx, "GET", project(m[1])+"/merge_requests/"+m[2], nil, &mr); err != nil {
		return MergedRefs{}, err
	}
	if mr.DiffRefs == nil || mr.DiffRefs.Base == "" || mr.DiffRefs.Head == "" {
		return MergedRefs{}, errors.New("GitLab did not tell the MR's commits")
	}
	iid := m[2]
	if mr.IID > 0 {
		iid = strconv.Itoa(mr.IID)
	}
	return MergedRefs{Base: mr.DiffRefs.Base, Head: mr.DiffRefs.Head, Ref: "refs/merge-requests/" + iid + "/head"}, nil
}
