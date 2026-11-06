package trackers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// linksEvery: how long the links GitLab gave of an issue are kept (an issue without updated_at: not at all). A blocker closed elsewhere does not change the
// issue it blocks, so the links are read again after a while even if the issue stays the same.
const linksEvery = 5 * time.Minute

// linkMemo: the open issues that block one issue, as GitLab said at a moment.
type linkMemo struct {
	updated  string
	at       time.Time
	blockers []string
}

// glLink: an issue linked to another, as GitLab's issue links list gives it.
type glLink struct {
	glIssue
	LinkType string `json:"link_type"`
}

// blockedBy: the open issues linked to this one as blocking it («is blocked by»). A project without such links
// (GitLab Free, links turned off) has none; a failed read keeps what was known (ok false).
func (g *GitLab) blockedBy(ctx context.Context, projectPath string, is glIssue, id string, now time.Time) (blockers []string, ok bool) {
	g.mu.Lock()
	m, known := g.links[id]
	g.mu.Unlock()
	if known && is.UpdatedAt != "" && m.updated == is.UpdatedAt && now.Sub(m.at) < linksEvery {
		return slices.Clone(m.blockers), true
	}
	var links []glLink
	err := g.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/links", project(projectPath), is.IID), nil, &links)
	var he *HTTPError
	if err != nil && !(errors.As(err, &he) && (he.Status == 403 || he.Status == 404)) {
		return nil, false
	}
	for _, l := range links {
		if l.LinkType != "is_blocked_by" || l.State != "opened" {
			continue
		}
		p := issueProject(l.glIssue)
		if p == "" {
			p = projectPath
		}
		blockers = append(blockers, p+"#"+strconv.Itoa(l.IID))
	}
	g.mu.Lock()
	if g.links == nil {
		g.links = map[string]linkMemo{}
	}
	g.links[id] = linkMemo{updated: is.UpdatedAt, at: now, blockers: blockers}
	g.mu.Unlock()
	return slices.Clone(blockers), true
}
