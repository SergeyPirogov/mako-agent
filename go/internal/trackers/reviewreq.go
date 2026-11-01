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

// --- GitLab

type glReviewMR struct {
	IID         int    `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	WebURL      string `json:"web_url"`
	SHA         string `json:"sha"`
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
	out := []core.ReviewRequest{}
	for _, m := range mrs {
		proj := m.References.Full
		if i := strings.LastIndex(proj, "!"); i > 0 {
			proj = proj[:i]
		} else if mm := mrURL.FindStringSubmatch(m.WebURL); mm != nil {
			proj = mm[1]
		}
		r := core.ReviewRequest{ID: m.WebURL, TrackerID: g.cfg.ID, Project: proj, Number: m.IID, Ref: fmt.Sprintf("!%d", m.IID),
			Title: m.Title, Description: m.Description, URL: m.WebURL, Author: m.Author.Username, HeadSHA: m.SHA}
		path := project(proj) + "/merge_requests/" + strconv.Itoa(m.IID)
		// The list has no pipeline: the MR itself has its head's.
		var one glMR
		if g.do(ctx, "GET", path, nil, &one) == nil && one.HeadPipeline != nil {
			r.CI = one.HeadPipeline.Status
		}
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
		r.Additions, r.Deletions = g.mrSize(ctx, path, m.SHA)
		out = append(out, r)
	}
	return out, nil
}

// mrSize: the lines the MR adds and removes, read once per head commit.
func (g *GitLab) mrSize(ctx context.Context, path, sha string) (int, int) {
	key := path + "@" + sha
	g.mu.Lock()
	if s, ok := g.sizes[key]; ok {
		g.mu.Unlock()
		return s[0], s[1]
	}
	g.mu.Unlock()
	diffs, err := allPages(url.Values{}, func(query string, out *[]glDiff) error { return g.do(ctx, "GET", path+"/diffs?"+query, nil, out) })
	if err != nil {
		return 0, 0
	}
	add, del := 0, 0
	for _, d := range diffs {
		a, r := countDiff(d.Diff)
		add, del = add+a, del+r
	}
	g.mu.Lock()
	if g.sizes == nil {
		g.sizes = map[string][2]int{}
	}
	g.sizes[key] = [2]int{add, del}
	g.mu.Unlock()
	return add, del
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
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
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
		var res struct {
			Items []ghSearchItem `json:"items"`
		}
		if err := g.do(ctx, "GET", "/search/issues?"+url.Values{"q": {q}, "per_page": {"100"}}.Encode(), nil, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			if strings.Contains(q, "review-requested") {
				requested[it.HTMLURL] = true
			}
			if !seen[it.HTMLURL] && !strings.EqualFold(it.User.Login, me) {
				seen[it.HTMLURL] = true
				items = append(items, it)
			}
		}
	}
	out := []core.ReviewRequest{}
	for _, it := range items {
		m := prURL.FindStringSubmatch(it.HTMLURL)
		if m == nil {
			continue
		}
		repo := m[1]
		r := core.ReviewRequest{ID: it.HTMLURL, TrackerID: g.cfg.ID, Project: repo, Number: it.Number, Ref: fmt.Sprintf("#%d", it.Number),
			Title: it.Title, Description: it.Body, URL: it.HTMLURL, Author: it.User.Login}
		path := fmt.Sprintf("/repos/%s/pulls/%d", repo, it.Number)
		var p ghPull
		if g.do(ctx, "GET", path, nil, &p) == nil {
			r.HeadSHA, r.Additions, r.Deletions = p.Head.SHA, p.Additions, p.Deletions
			r.CI, _ = g.checkRuns(ctx, repo, p.Head.SHA)
		}
		// Approved: my last say on it was an approval, and nobody asked me to look again since.
		if !requested[it.HTMLURL] {
			var reviews []struct {
				State string `json:"state"`
				User  struct {
					Login string `json:"login"`
				} `json:"user"`
			}
			if g.do(ctx, "GET", path+"/reviews?per_page=100", nil, &reviews) == nil {
				last := ""
				for _, rv := range reviews {
					if strings.EqualFold(rv.User.Login, me) && rv.State != "COMMENTED" && rv.State != "PENDING" {
						last = rv.State
					}
				}
				r.Approved = last == "APPROVED"
			}
		}
		out = append(out, r)
	}
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
