package trackers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// My review of someone else's MR, as Mako's reviewer prepared it: the comments I chose go to the MR from me,
// each on its line where the diff shows it, and my approval.

// ReviewPoster is a code host that takes my review of an MR: comments on its lines, and approval.
type ReviewPoster interface {
	// PostReview posts the notes on the MR's head (headSHA, "" the MR's own); a result per note, in their order.
	// The error is the review's as a whole (no such MR): then no note was posted.
	PostReview(ctx context.Context, reviewURL, headSHA string, notes []DraftNote) ([]PostedNote, error)
	ApproveReview(ctx context.Context, reviewURL string) error
}

// DraftNote is a comment to post: on Path at Line of the new version when the diff shows that line, else a
// comment on the MR that names the place.
type DraftNote struct {
	Path string
	Line int
	Body string
}

// PostedNote: where the note went (URL), or why it did not (Error).
type PostedNote struct {
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

var hunkHead = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// inDiff: the file's line (in its new version) is one the diff shows, added or around a change: a code host
// takes a comment on it there.
func inDiff(files []ReviewFile, path string, line int) bool {
	if line <= 0 {
		return false
	}
	for _, f := range files {
		if f.Path != path || f.Status == "D" {
			continue
		}
		n := 0
		for _, l := range strings.Split(f.Diff, "\n") {
			if m := hunkHead.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
				continue
			}
			if n == 0 {
				continue
			}
			// Removed lines ("-") and «\ No newline» are not of the new version.
			if strings.HasPrefix(l, "+") || strings.HasPrefix(l, " ") {
				if n == line {
					return true
				}
				n++
			}
		}
	}
	return false
}

// placed: the note's body for a comment on the MR as a whole: the place it is about first.
func placed(n DraftNote) string {
	if n.Path == "" {
		return n.Body
	}
	where := n.Path
	if n.Line > 0 {
		where += ":" + strconv.Itoa(n.Line)
	}
	return "`" + where + "` — " + n.Body
}

func oldPathOf(files []ReviewFile, path string) string {
	for _, f := range files {
		if f.Path == path && f.OldPath != "" {
			return f.OldPath
		}
	}
	return path
}

// --- GitLab: a discussion per note, on its line of the diff where it can be.

func (g *GitLab) PostReview(ctx context.Context, reviewURL, headSHA string, notes []DraftNote) ([]PostedNote, error) {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return nil, fmt.Errorf("not a GitLab MR: %s", reviewURL)
	}
	path := project(m[1]) + "/merge_requests/" + m[2]
	var mr struct {
		DiffRefs *struct {
			Base  string `json:"base_sha"`
			Start string `json:"start_sha"`
			Head  string `json:"head_sha"`
		} `json:"diff_refs"`
	}
	if err := g.do(ctx, "GET", path, nil, &mr); err != nil {
		return nil, err
	}
	// The lines are the ones of the head the notes were written on: an MR pushed since gets them as general notes.
	refs := mr.DiffRefs
	if refs != nil && headSHA != "" && refs.Head != headSHA {
		refs = nil
	}
	var files []ReviewFile
	if refs != nil {
		files, _ = g.ReviewFiles(ctx, reviewURL)
	}
	out := make([]PostedNote, len(notes))
	for i, n := range notes {
		var d struct {
			Notes []glNote `json:"notes"`
		}
		var err error
		posted := false
		if refs != nil && inDiff(files, n.Path, n.Line) {
			body := map[string]any{"body": n.Body, "position": map[string]any{
				"position_type": "text", "base_sha": refs.Base, "start_sha": refs.Start, "head_sha": refs.Head,
				"old_path": oldPathOf(files, n.Path), "new_path": n.Path, "new_line": n.Line,
			}}
			err = g.do(ctx, "POST", path+"/discussions", body, &d)
			var he *HTTPError
			// GitLab tells a line it will not take by 400: the note goes to the MR as a whole then.
			posted = err == nil || !(errors.As(err, &he) && he.Status == 400)
		}
		if !posted {
			err = g.do(ctx, "POST", path+"/discussions", map[string]any{"body": placed(n)}, &d)
		}
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		if len(d.Notes) > 0 {
			out[i].URL = noteLink(reviewURL, d.Notes[0].ID)
		}
		if out[i].URL == "" {
			out[i].URL = reviewURL
		}
	}
	return out, nil
}

func (g *GitLab) ApproveReview(ctx context.Context, reviewURL string) error {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("not a GitLab MR: %s", reviewURL)
	}
	return g.do(ctx, "POST", project(m[1])+"/merge_requests/"+m[2]+"/approve", map[string]any{}, nil)
}

// --- GitHub: one review with the notes on their lines, the others in its body.

func (g *GitHub) PostReview(ctx context.Context, reviewURL, headSHA string, notes []DraftNote) ([]PostedNote, error) {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return nil, fmt.Errorf("not a GitHub PR: %s", reviewURL)
	}
	files, err := g.ReviewFiles(ctx, reviewURL)
	if err != nil {
		return nil, err
	}
	post := func(inline bool) (string, error) {
		var general []string
		comments := []map[string]any{}
		for _, n := range notes {
			if inline && inDiff(files, n.Path, n.Line) {
				comments = append(comments, map[string]any{"path": n.Path, "line": n.Line, "side": "RIGHT", "body": n.Body})
			} else {
				general = append(general, placed(n))
			}
		}
		body := map[string]any{"event": "COMMENT", "body": strings.Join(general, "\n\n"), "comments": comments}
		if headSHA != "" {
			body["commit_id"] = headSHA
		}
		var res struct {
			HTMLURL string `json:"html_url"`
		}
		err := g.do(ctx, "POST", "/repos/"+m[1]+"/pulls/"+m[2]+"/reviews", body, &res)
		if res.HTMLURL == "" {
			res.HTMLURL = reviewURL
		}
		return res.HTMLURL, err
	}
	link, err := post(true)
	var he *HTTPError
	// 422: GitHub will not take a line (the PR was pushed since): the whole review goes as its body then.
	if errors.As(err, &he) && he.Status == 422 {
		link, err = post(false)
	}
	out := make([]PostedNote, len(notes))
	for i := range out {
		if err != nil {
			out[i].Error = err.Error()
		} else {
			out[i].URL = link
		}
	}
	return out, nil
}

func (g *GitHub) ApproveReview(ctx context.Context, reviewURL string) error {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("not a GitHub PR: %s", reviewURL)
	}
	return g.do(ctx, "POST", "/repos/"+m[1]+"/pulls/"+m[2]+"/reviews", map[string]any{"event": "APPROVE"}, nil)
}
