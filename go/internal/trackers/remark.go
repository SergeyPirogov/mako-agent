package trackers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Commenter: a tracker that tells the link to the comment it leaves (the reviewer's comments link to it).
type Commenter interface {
	CommentLink(ctx context.Context, t core.Task, text string) (string, error)
}

// ReviewCommenter: a code host that comments on a merge request.
type ReviewCommenter interface {
	CommentReview(ctx context.Context, reviewURL, text string) (string, error)
}

func (g *GitHub) CommentLink(ctx context.Context, t core.Task, text string) (string, error) {
	var c struct {
		HTMLURL string `json:"html_url"`
	}
	err := g.do(ctx, "POST", ghIssuePath(t)+"/comments", map[string]any{"body": text}, &c)
	return c.HTMLURL, err
}

// CommentReview: a PR's conversation is its issue's.
func (g *GitHub) CommentReview(ctx context.Context, reviewURL, text string) (string, error) {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return "", fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	var c struct {
		HTMLURL string `json:"html_url"`
	}
	err := g.do(ctx, "POST", "/repos/"+m[1]+"/issues/"+m[2]+"/comments", map[string]any{"body": text}, &c)
	return c.HTMLURL, err
}

// glNote: what GitLab answers for a note; its link is the page's with #note_<id>.
type glNote struct {
	ID int `json:"id"`
}

func noteLink(page string, id int) string {
	if page == "" || id == 0 {
		return ""
	}
	return strings.SplitN(page, "#", 2)[0] + "#note_" + strconv.Itoa(id)
}

func (g *GitLab) CommentLink(ctx context.Context, t core.Task, text string) (string, error) {
	var n glNote
	err := g.do(ctx, "POST", issuePath(t)+"/notes", map[string]any{"body": text}, &n)
	return noteLink(t.URL, n.ID), err
}

func (g *GitLab) CommentReview(ctx context.Context, reviewURL, text string) (string, error) {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return "", fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	var n glNote
	err := g.do(ctx, "POST", project(m[1])+"/merge_requests/"+m[2]+"/notes", map[string]any{"body": text}, &n)
	return noteLink(reviewURL, n.ID), err
}

func (j *Jira) CommentLink(ctx context.Context, t core.Task, text string) (string, error) {
	var c struct {
		ID string `json:"id"`
	}
	if err := j.do(ctx, "POST", "/rest/api/3/issue/"+t.ID+"/comment", map[string]any{"body": textToADF(text)}, &c); err != nil {
		return "", err
	}
	if c.ID == "" {
		return "", nil
	}
	return strings.TrimRight(j.base, "/") + "/browse/" + t.ID + "?focusedCommentId=" + c.ID, nil
}
