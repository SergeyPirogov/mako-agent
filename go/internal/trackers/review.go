package trackers

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The MR next to the agent: CI, review threads, conflicts and the merge, for the board's MR card.

// ReviewCheck: what an MR needs before it can be merged.
type ReviewCheck struct {
	State     string   `json:"state"` // "open", "merged", "closed"
	CI        string   `json:"ci"`    // "", "running", "success", "failed", "canceled", "manual"
	CIURL     string   `json:"ciUrl,omitempty"`
	Jobs      []Job    `json:"jobs"`    // the pipeline's jobs in their stages' order
	Failed    []Job    `json:"failed"`  // the jobs that failed and may not
	Threads   []Thread `json:"threads"` // review threads nobody resolved yet
	Conflicts bool     `json:"conflicts"`
	Mergeable bool     `json:"mergeable"`
	Blocked   string   `json:"blocked,omitempty"` // why it cannot be merged yet, in words
}

type Job struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Stage string `json:"stage,omitempty"`
	// "success", "failed", "running", "pending", "canceled", "skipped", "manual"
	Status       string `json:"status"`
	AllowFailure bool   `json:"allowFailure,omitempty"`
}

type Thread struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	URL    string `json:"url"`
}

// ReviewDesk: a code host that can show and merge its MRs.
type ReviewDesk interface {
	CheckReview(ctx context.Context, url string) (*ReviewCheck, error)
	// JobLog: the end of a failed job's log, last lines.
	JobLog(ctx context.Context, url string, job Job, lines int) (string, error)
	Merge(ctx context.Context, url string) error
	// Close closes the MR without merging it.
	Close(ctx context.Context, url string) error
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\r`)

func tail(log string, n int) string {
	log = ansi.ReplaceAllString(log, "")
	ls := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// --- GitLab

var glBlocked = map[string]string{
	"ci_must_pass": "спершу має пройти CI", "ci_still_running": "CI ще йде", "discussions_not_resolved": "є нерозвʼязані коментарі",
	"not_approved": "потрібен апрув", "conflict": "конфлікти з основною гілкою", "draft_status": "MR — чернетка",
	"need_rebase": "потрібен rebase", "checking": "GitLab ще перевіряє", "unchecked": "GitLab ще перевіряє",
	"blocked_status": "MR заблоковано іншим", "requested_changes": "рецензент попросив зміни", "not_open": "MR не відкритий",
}

func (g *GitLab) CheckReview(ctx context.Context, reviewURL string) (*ReviewCheck, error) {
	path, mr, err := g.mr(ctx, reviewURL)
	if err != nil {
		return nil, err
	}
	c := &ReviewCheck{State: mrState(mr.State), Jobs: []Job{}, Failed: []Job{}, Threads: []Thread{}}
	if p := mr.HeadPipeline; p != nil {
		c.CI, c.CIURL = p.Status, p.WebURL
		var jobs []struct {
			ID           int64  `json:"id"`
			Name         string `json:"name"`
			Stage        string `json:"stage"`
			Status       string `json:"status"`
			WebURL       string `json:"web_url"`
			AllowFailure bool   `json:"allow_failure"`
		}
		m := mrURL.FindStringSubmatch(reviewURL)
		if err := g.do(ctx, "GET", fmt.Sprintf("%s/pipelines/%d/jobs?per_page=100", project(m[1]), p.ID), nil, &jobs); err == nil {
			// GitLab lists the newest first: in run order, the stages come in the pipeline's order.
			for i := len(jobs) - 1; i >= 0; i-- {
				j := jobs[i]
				job := Job{ID: j.ID, Name: j.Name, URL: j.WebURL, Stage: j.Stage, Status: j.Status, AllowFailure: j.AllowFailure}
				c.Jobs = append(c.Jobs, job)
				if j.Status == "failed" && !j.AllowFailure {
					c.Failed = append(c.Failed, job)
				}
			}
		}
	}
	var discussions []struct {
		Notes []struct {
			ID         int64  `json:"id"`
			Body       string `json:"body"`
			System     bool   `json:"system"`
			Resolvable bool   `json:"resolvable"`
			Resolved   bool   `json:"resolved"`
			Author     struct {
				Username string `json:"username"`
			} `json:"author"`
			Position *struct {
				NewPath string `json:"new_path"`
				NewLine int    `json:"new_line"`
			} `json:"position"`
		} `json:"notes"`
	}
	if err := g.do(ctx, "GET", path+"/discussions?per_page=100", nil, &discussions); err == nil {
		for _, d := range discussions {
			if len(d.Notes) == 0 || d.Notes[0].System || !d.Notes[0].Resolvable || d.Notes[0].Resolved {
				continue
			}
			n := d.Notes[0]
			t := Thread{Author: n.Author.Username, Body: n.Body, URL: fmt.Sprintf("%s#note_%d", mr.WebURL, n.ID)}
			if n.Position != nil {
				t.Path, t.Line = n.Position.NewPath, n.Position.NewLine
			}
			c.Threads = append(c.Threads, t)
		}
	}
	c.Conflicts = mr.HasConflicts || mr.MergeStatus == "conflict"
	c.Mergeable = c.State == "open" && mr.MergeStatus == "mergeable"
	if c.State == "open" && !c.Mergeable {
		c.Blocked = glBlocked[mr.MergeStatus]
		if c.Blocked == "" {
			c.Blocked = strings.ReplaceAll(mr.MergeStatus, "_", " ")
		}
	}
	return c, nil
}

func (g *GitLab) JobLog(ctx context.Context, reviewURL string, job Job, lines int) (string, error) {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return "", fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	var log string
	if err := g.do(ctx, "GET", fmt.Sprintf("%s/jobs/%d/trace", project(m[1]), job.ID), nil, &log); err != nil {
		return "", err
	}
	return tail(log, lines), nil
}

func (g *GitLab) Merge(ctx context.Context, reviewURL string) error {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	return g.do(ctx, "PUT", project(m[1])+"/merge_requests/"+m[2]+"/merge", map[string]any{"should_remove_source_branch": true}, nil)
}

func (g *GitLab) Close(ctx context.Context, reviewURL string) error {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	return g.do(ctx, "PUT", project(m[1])+"/merge_requests/"+m[2], map[string]any{"state_event": "close"}, nil)
}

// --- GitHub

var ghBlocked = map[string]string{
	"blocked": "потрібні перевірки або апрув", "behind": "гілка відстала від основної", "dirty": "конфлікти з основною гілкою",
	"draft": "PR — чернетка", "unknown": "GitHub ще перевіряє",
}

func (g *GitHub) CheckReview(ctx context.Context, reviewURL string) (*ReviewCheck, error) {
	_, p, err := g.pull(ctx, reviewURL)
	if err != nil {
		return nil, err
	}
	m := prURL.FindStringSubmatch(reviewURL)
	repo, num := m[1], m[2]
	c := &ReviewCheck{State: p.state(), Jobs: []Job{}, Failed: []Job{}, Threads: []Thread{}}
	var runs struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"check_runs"`
	}
	if p.Head.SHA != "" && g.do(ctx, "GET", "/repos/"+repo+"/commits/"+p.Head.SHA+"/check-runs?per_page=100", nil, &runs) == nil && len(runs.CheckRuns) > 0 {
		c.CI, c.CIURL = "success", reviewURL+"/checks"
		running := false
		for _, r := range runs.CheckRuns {
			job := Job{ID: r.ID, Name: r.Name, URL: r.HTMLURL, Status: "success"}
			switch {
			case r.Status != "completed":
				running = true
				job.Status = map[bool]string{true: "running", false: "pending"}[r.Status == "in_progress"]
			case r.Conclusion == "failure" || r.Conclusion == "timed_out" || r.Conclusion == "cancelled" || r.Conclusion == "action_required":
				job.Status = "failed"
				c.Failed = append(c.Failed, job)
			case r.Conclusion == "skipped" || r.Conclusion == "neutral":
				job.Status = "skipped"
			}
			c.Jobs = append(c.Jobs, job)
		}
		if len(c.Failed) > 0 {
			c.CI = "failed"
		} else if running {
			c.CI = "running"
		}
	}
	owner, name, _ := strings.Cut(repo, "/")
	n, _ := strconv.Atoi(num)
	var threads struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool   `json:"isResolved"`
							Path       string `json:"path"`
							Line       int    `json:"line"`
							Comments   struct {
								Nodes []struct {
									Body   string `json:"body"`
									URL    string `json:"url"`
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	query := `query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){reviewThreads(first:100){nodes{isResolved path line comments(first:1){nodes{body url author{login}}}}}}}}`
	if call(ctx, "POST", g.graphqlURL(), g.headers, map[string]any{"query": query, "variables": map[string]any{"o": owner, "n": name, "p": n}}, &threads, nil) == nil {
		for _, t := range threads.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if t.IsResolved || len(t.Comments.Nodes) == 0 {
				continue
			}
			first := t.Comments.Nodes[0]
			c.Threads = append(c.Threads, Thread{Author: first.Author.Login, Body: first.Body, Path: t.Path, Line: t.Line, URL: first.URL})
		}
	}
	c.Conflicts = p.MergeableState == "dirty"
	c.Mergeable = c.State == "open" && p.Mergeable != nil && *p.Mergeable && (p.MergeableState == "clean" || p.MergeableState == "unstable" || p.MergeableState == "has_hooks")
	if c.State == "open" && !c.Mergeable {
		c.Blocked = ghBlocked[p.MergeableState]
		if c.Blocked == "" {
			c.Blocked = "GitHub ще перевіряє"
		}
	}
	return c, nil
}

// graphqlURL: api.github.com/graphql; GitHub Enterprise's /api/v3 has /api/graphql.
func (g *GitHub) graphqlURL() string {
	if strings.HasSuffix(g.api, "/api/v3") {
		return strings.TrimSuffix(g.api, "/v3") + "/graphql"
	}
	return g.api + "/graphql"
}

func (g *GitHub) JobLog(ctx context.Context, reviewURL string, job Job, lines int) (string, error) {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return "", fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	// A check run of GitHub Actions is its job: the log is there; other checks have none.
	var log string
	if err := g.do(ctx, "GET", "/repos/"+m[1]+"/actions/jobs/"+strconv.FormatInt(job.ID, 10)+"/logs", nil, &log); err != nil {
		return "", err
	}
	return tail(log, lines), nil
}

func (g *GitHub) Merge(ctx context.Context, reviewURL string) error {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	return g.do(ctx, "PUT", "/repos/"+m[1]+"/pulls/"+m[2]+"/merge", map[string]any{}, nil)
}

func (g *GitHub) Close(ctx context.Context, reviewURL string) error {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	return g.do(ctx, "PATCH", "/repos/"+m[1]+"/pulls/"+m[2], map[string]any{"state": "closed"}, nil)
}
