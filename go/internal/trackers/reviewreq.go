package trackers

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Merge requests of others where I am a reviewer: «Рев'ю для мене» on the board.

// ReviewInbox is a code host that can tell which MRs wait for my review.
type ReviewInbox interface {
	// ToReview: the open MRs where I am a reviewer, with their size, CI, head commit and whether I approved.
	ToReview(ctx context.Context) ([]core.ReviewRequest, error)
	// ReviewState: "open", "merged" or "closed" (as CodeHost's).
	ReviewState(ctx context.Context, url string) (string, error)
	// ReviewFiles: what the MR changes, file by file, with each file's diff.
	ReviewFiles(ctx context.Context, url string) ([]ReviewFile, error)
	CheckReview(ctx context.Context, url string) (*ReviewCheck, error)
}

// ReviewFile is one file of an MR's diff.
type ReviewFile struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"` // a renamed file's old name
	// "A" added, "M" modified, "D" deleted, "R" renamed
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Diff      string `json:"diff"` // its hunks, "@@ -1,2 +1,3 @@ …"; "" when too big or binary
}

// countDiff: the lines a diff's hunks add and remove.
func countDiff(diff string) (add, del int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "), strings.HasPrefix(l, "--- "):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
}

// reviewMemo is what Mako remembers of an MR to review between polls, so each poll asks the code host only
// what may have changed: the size per head commit, CI until it settles, GitHub's PR and reviews per update.
type reviewMemo struct {
	sha, updated string
	sized        bool
	add, del     int
	ci           string
	approved     bool // my last say on it was an approval (GitHub)
}

// ciSettled: the head's CI ended and will not change by itself.
func ciSettled(ci string) bool { return ci != "" && !CIRunning(ci) }

// --- GitLab

type glReviewMR struct {
	IID         int    `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	WebURL      string `json:"web_url"`
	SHA         string `json:"sha"`
	CreatedAt   string `json:"created_at"`
	Author      glUser `json:"author"`
	References  struct {
		Full string `json:"full"` // "group/app!12"
	} `json:"references"`
}

type glDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	DeletedFile bool   `json:"deleted_file"`
	RenamedFile bool   `json:"renamed_file"`
	Diff        string `json:"diff"`
}

func (g *GitLab) ToReview(ctx context.Context) ([]core.ReviewRequest, error) {
	me, err := g.Me(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"reviewer_username": {me}, "state": {"opened"}, "scope": {"all"}}
	mrs, err := allPages(q, func(query string, out *[]glReviewMR) error {
		return g.do(ctx, "GET", "/merge_requests?"+query, nil, out)
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	old := g.memos
	g.mu.Unlock()
	memos := map[string]reviewMemo{}
	out := []core.ReviewRequest{}
	for _, m := range mrs {
		if m.Author.Username == me {
			continue // my own MR, with me among its reviewers
		}
		proj := m.References.Full
		if i := strings.LastIndex(proj, "!"); i > 0 {
			proj = proj[:i]
		} else if mm := mrURL.FindStringSubmatch(m.WebURL); mm != nil {
			proj = mm[1]
		}
		r := core.ReviewRequest{ID: m.WebURL, TrackerID: g.cfg.ID, Project: proj, Number: m.IID, Ref: fmt.Sprintf("!%d", m.IID),
			Title: m.Title, Description: m.Description, URL: m.WebURL, Author: m.Author.Username, HeadSHA: m.SHA, Since: m.CreatedAt}
		path := project(proj) + "/merge_requests/" + strconv.Itoa(m.IID)
		memo := old[path]
		if memo.sha != m.SHA {
			memo = reviewMemo{sha: m.SHA}
		}
		// The list has no pipeline: the MR itself has its head's, asked until it settles.
		if !ciSettled(memo.ci) {
			var one glMR
			if g.do(ctx, "GET", path, nil, &one) == nil && one.HeadPipeline != nil {
				memo.ci = one.HeadPipeline.Status
			}
		}
		if !memo.sized {
			diffs, err := allPages(url.Values{}, func(query string, out *[]glDiff) error {
				return g.do(ctx, "GET", path+"/diffs?"+query, nil, out)
			})
			if err == nil {
				memo.sized, memo.add, memo.del = true, 0, 0
				for _, d := range diffs {
					a, r := countDiff(d.Diff)
					memo.add, memo.del = memo.add+a, memo.del+r
				}
			}
		}
		// An approval does not change the MR's head: asked every poll.
		var approvals struct {
			ApprovedBy []struct {
				User glUser `json:"user"`
			} `json:"approved_by"`
		}
		if g.do(ctx, "GET", path+"/approvals", nil, &approvals) == nil {
			for _, a := range approvals.ApprovedBy {
				if a.User.Username == me {
					r.Approved = true
				}
			}
		}
		memos[path] = memo
		r.CI, r.Additions, r.Deletions = memo.ci, memo.add, memo.del
		out = append(out, r)
	}
	// Only the MRs still to review are remembered.
	g.mu.Lock()
	g.memos = memos
	g.mu.Unlock()
	return out, nil
}

func (g *GitLab) ReviewFiles(ctx context.Context, reviewURL string) ([]ReviewFile, error) {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return nil, fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	diffs, err := allPages(url.Values{}, func(query string, out *[]glDiff) error {
		return g.do(ctx, "GET", project(m[1])+"/merge_requests/"+m[2]+"/diffs?"+query, nil, out)
	})
	if err != nil {
		return nil, err
	}
	files := []ReviewFile{}
	for _, d := range diffs {
		f := ReviewFile{Path: d.NewPath, Status: "M", Diff: d.Diff}
		switch {
		case d.NewFile:
			f.Status = "A"
		case d.DeletedFile:
			f.Status, f.Path = "D", d.OldPath
		case d.RenamedFile:
			f.Status, f.OldPath = "R", d.OldPath
		}
		f.Additions, f.Deletions = countDiff(d.Diff)
		files = append(files, f)
	}
	return files, nil
}

// --- GitHub

type ghSearchItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// search reads all pages of a search (GitHub gives at most 1000 results); an incomplete answer is an error,
// or the PRs missing from it would look gone.
func (g *GitHub) search(ctx context.Context, q string) ([]ghSearchItem, error) {
	var all []ghSearchItem
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Total      int            `json:"total_count"`
			Incomplete bool           `json:"incomplete_results"`
			Items      []ghSearchItem `json:"items"`
		}
		query := url.Values{"q": {q}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := g.do(ctx, "GET", "/search/issues?"+query.Encode(), nil, &res); err != nil {
			return nil, err
		}
		if res.Incomplete {
			return nil, fmt.Errorf("GitHub дав неповну відповідь на пошук «%s»", q)
		}
		all = append(all, res.Items...)
		if len(res.Items) < perPage || len(all) >= res.Total {
			break
		}
	}
	return all, nil
}

// ToReview: the open PRs whose review is asked of me, and those I already reviewed that are not merged yet.
func (g *GitHub) ToReview(ctx context.Context) ([]core.ReviewRequest, error) {
	me, err := g.Me(ctx)
	if err != nil {
		return nil, err
	}
	requested := map[string]bool{}
	seen := map[string]bool{}
	var items []ghSearchItem
	for _, q := range []string{"is:pr is:open review-requested:@me", "is:pr is:open reviewed-by:@me"} {
		found, err := g.search(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, it := range found {
			if strings.Contains(q, "review-requested") {
				requested[it.HTMLURL] = true
			}
			if !seen[it.HTMLURL] && !strings.EqualFold(it.User.Login, me) {
				seen[it.HTMLURL] = true
				items = append(items, it)
			}
		}
	}
	g.mu.Lock()
	old := g.memos
	g.mu.Unlock()
	memos := map[string]reviewMemo{}
	out := []core.ReviewRequest{}
	for _, it := range items {
		m := prURL.FindStringSubmatch(it.HTMLURL)
		if m == nil {
			continue
		}
		repo := m[1]
		r := core.ReviewRequest{ID: it.HTMLURL, TrackerID: g.cfg.ID, Project: repo, Number: it.Number, Ref: fmt.Sprintf("#%d", it.Number),
			Title: it.Title, Description: it.Body, URL: it.HTMLURL, Author: it.User.Login, Since: it.CreatedAt}
		path := fmt.Sprintf("/repos/%s/pulls/%d", repo, it.Number)
		memo := old[it.HTMLURL]
		// A push, a review or a new request updates the PR: only then is it read again.
		if memo.updated != it.UpdatedAt || it.UpdatedAt == "" {
			var p ghPull
			if err := g.do(ctx, "GET", path, nil, &p); err == nil {
				if p.Head.SHA != memo.sha {
					memo.ci = ""
				}
				memo.sha, memo.add, memo.del, memo.sized = p.Head.SHA, p.Additions, p.Deletions, true
				var reviews []struct {
					State string `json:"state"`
					User  struct {
						Login string `json:"login"`
					} `json:"user"`
				}
				if err := g.do(ctx, "GET", path+"/reviews?per_page=100", nil, &reviews); err == nil {
					last := ""
					for _, rv := range reviews {
						if strings.EqualFold(rv.User.Login, me) && rv.State != "COMMENTED" && rv.State != "PENDING" {
							last = rv.State
						}
					}
					memo.approved = last == "APPROVED"
					memo.updated = it.UpdatedAt // read whole: not again until it changes
				}
			}
		}
		if memo.sha != "" && !ciSettled(memo.ci) {
			memo.ci, _ = g.checkRuns(ctx, repo, memo.sha)
		}
		memos[it.HTMLURL] = memo
		r.HeadSHA, r.Additions, r.Deletions, r.CI = memo.sha, memo.add, memo.del, memo.ci
		// Approved: my last say on it was an approval, and nobody asked me to look again since.
		r.Approved = memo.approved && !requested[it.HTMLURL]
		out = append(out, r)
	}
	g.mu.Lock()
	g.memos = memos
	g.mu.Unlock()
	return out, nil
}

// checkRuns: the head commit's checks in one word (as ReviewCheck.CI) and as jobs; "" when it has none.
func (g *GitHub) checkRuns(ctx context.Context, repo, sha string) (string, []Job) {
	var runs struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"check_runs"`
	}
	if sha == "" || g.do(ctx, "GET", "/repos/"+repo+"/commits/"+sha+"/check-runs?per_page=100", nil, &runs) != nil || len(runs.CheckRuns) == 0 {
		return "", nil
	}
	ci, running, failed := "success", false, false
	jobs := []Job{}
	for _, r := range runs.CheckRuns {
		job := Job{ID: r.ID, Name: r.Name, URL: r.HTMLURL, Status: "success"}
		switch {
		case r.Status != "completed":
			running = true
			job.Status = map[bool]string{true: "running", false: "pending"}[r.Status == "in_progress"]
		case r.Conclusion == "failure" || r.Conclusion == "timed_out" || r.Conclusion == "cancelled" || r.Conclusion == "action_required":
			job.Status, failed = "failed", true
		case r.Conclusion == "skipped" || r.Conclusion == "neutral":
			job.Status = "skipped"
		}
		jobs = append(jobs, job)
	}
	if failed {
		ci = "failed"
	} else if running {
		ci = "running"
	}
	return ci, jobs
}

func (g *GitHub) ReviewFiles(ctx context.Context, reviewURL string) ([]ReviewFile, error) {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return nil, fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	type ghFile struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
		Status           string `json:"status"`
		Additions        int    `json:"additions"`
		Deletions        int    `json:"deletions"`
		Patch            string `json:"patch"`
	}
	list, err := allPages(url.Values{}, func(query string, out *[]ghFile) error {
		return g.do(ctx, "GET", "/repos/"+m[1]+"/pulls/"+m[2]+"/files?"+query, nil, out)
	})
	if err != nil {
		return nil, err
	}
	files := []ReviewFile{}
	for _, f := range list {
		st := map[string]string{"added": "A", "removed": "D", "renamed": "R"}[f.Status]
		if st == "" {
			st = "M"
		}
		files = append(files, ReviewFile{Path: f.Filename, OldPath: f.PreviousFilename, Status: st, Additions: f.Additions, Deletions: f.Deletions, Diff: f.Patch})
	}
	return files, nil
}
