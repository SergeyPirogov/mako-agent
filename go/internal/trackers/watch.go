package trackers

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The project's default branch as Mako watches it: its CI and the security alerts of its dependencies.

// BranchCI: the newest pipeline (GitLab) or the head commit's checks (GitHub) of a branch.
type BranchCI struct {
	Branch string
	SHA    string
	CI     string // as ReviewCheck.CI: "", "running", "success", "failed", …
	URL    string // the pipeline, the commit's checks
	Failed []Job  // the jobs that failed and may not
}

type Vulnerability struct {
	ID       string
	Package  string
	Severity string
	Summary  string
	URL      string
}

// Watcher: a code host whose default branch Mako can watch (GitLab, GitHub).
type Watcher interface {
	DefaultCI(ctx context.Context, project string) (*BranchCI, error)
	// BranchJobLog: the end of a failed job's log on the default branch, last lines.
	BranchJobLog(ctx context.Context, project string, job Job, lines int) (string, error)
	// Vulnerabilities: the open security alerts of the project.
	Vulnerabilities(ctx context.Context, project string) ([]Vulnerability, error)
}

// --- GitHub

func (g *GitHub) DefaultCI(ctx context.Context, repo string) (*BranchCI, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, "GET", "/repos/"+repo, nil, &r); err != nil {
		return nil, err
	}
	var commit struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.do(ctx, "GET", "/repos/"+repo+"/commits/"+url.PathEscape(r.DefaultBranch), nil, &commit); err != nil {
		return nil, err
	}
	b := &BranchCI{Branch: r.DefaultBranch, SHA: commit.SHA, Failed: []Job{}}
	ci, jobs := g.checkRuns(ctx, repo, commit.SHA)
	b.CI = ci
	if commit.HTMLURL != "" {
		b.URL = commit.HTMLURL + "/checks"
	}
	for _, j := range jobs {
		if j.Status == "failed" {
			b.Failed = append(b.Failed, j)
		}
	}
	return b, nil
}

func (g *GitHub) BranchJobLog(ctx context.Context, repo string, job Job, lines int) (string, error) {
	if job.ID == 0 {
		return "", nil // a commit status (Jenkins, Travis) has no Actions log
	}
	var log string
	if err := g.do(ctx, "GET", "/repos/"+repo+"/actions/jobs/"+strconv.FormatInt(job.ID, 10)+"/logs", nil, &log); err != nil {
		return "", err
	}
	return tail(log, lines), nil
}

// Vulnerabilities: the open Dependabot alerts; the token needs the security_events (or Dependabot alerts) right.
func (g *GitHub) Vulnerabilities(ctx context.Context, repo string) ([]Vulnerability, error) {
	var alerts []struct {
		Number   int    `json:"number"`
		HTMLURL  string `json:"html_url"`
		Advisory struct {
			Summary  string `json:"summary"`
			Severity string `json:"severity"`
		} `json:"security_advisory"`
		Vulnerability struct {
			Severity string `json:"severity"`
			Package  struct {
				Name string `json:"name"`
			} `json:"package"`
		} `json:"security_vulnerability"`
	}
	if err := g.do(ctx, "GET", "/repos/"+repo+"/dependabot/alerts?state=open&per_page=100", nil, &alerts); err != nil {
		return nil, err
	}
	out := []Vulnerability{}
	for _, a := range alerts {
		sev := a.Vulnerability.Severity
		if sev == "" {
			sev = a.Advisory.Severity
		}
		out = append(out, Vulnerability{ID: strconv.Itoa(a.Number), Package: a.Vulnerability.Package.Name, Severity: sev, Summary: a.Advisory.Summary, URL: a.HTMLURL})
	}
	return out, nil
}

// --- GitLab

func (g *GitLab) DefaultCI(ctx context.Context, path string) (*BranchCI, error) {
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, "GET", project(path), nil, &p); err != nil {
		return nil, err
	}
	var pipelines []struct {
		ID     int64  `json:"id"`
		SHA    string `json:"sha"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	}
	if err := g.do(ctx, "GET", project(path)+"/pipelines?per_page=1&ref="+url.QueryEscape(p.DefaultBranch), nil, &pipelines); err != nil {
		return nil, err
	}
	b := &BranchCI{Branch: p.DefaultBranch, Failed: []Job{}}
	if len(pipelines) == 0 {
		return b, nil
	}
	pl := pipelines[0]
	b.SHA, b.CI, b.URL = pl.SHA, pl.Status, pl.WebURL
	if pl.Status == "failed" {
		_, failed, err := g.pipelineJobs(ctx, path, pl.ID)
		if err != nil {
			return nil, err
		}
		b.Failed = failed
	}
	return b, nil
}

func (g *GitLab) BranchJobLog(ctx context.Context, path string, job Job, lines int) (string, error) {
	var log string
	if err := g.do(ctx, "GET", fmt.Sprintf("%s/jobs/%d/trace", project(path), job.ID), nil, &log); err != nil {
		return "", err
	}
	return tail(log, lines), nil
}

// Vulnerabilities: the detected and confirmed vulnerabilities of the project (GitLab Ultimate's security
// dashboard).
func (g *GitLab) Vulnerabilities(ctx context.Context, path string) ([]Vulnerability, error) {
	var res struct {
		Project *struct {
			Vulnerabilities *struct {
				Nodes []struct {
					ID          string `json:"id"`
					Title       string `json:"title"`
					Severity    string `json:"severity"`
					WebURL      string `json:"webUrl"`
					Description string `json:"description"`
				} `json:"nodes"`
			} `json:"vulnerabilities"`
		} `json:"project"`
	}
	query := `query($p:ID!){project(fullPath:$p){vulnerabilities(state:[DETECTED,CONFIRMED],first:100){nodes{id title severity webUrl}}}}`
	if err := g.graphql(ctx, query, map[string]any{"p": path}, &res); err != nil {
		return nil, err
	}
	if res.Project == nil {
		return nil, fmt.Errorf("GitLab: no project %s", path)
	}
	if res.Project.Vulnerabilities == nil {
		return nil, fmt.Errorf("GitLab gives no vulnerabilities of %s: they need GitLab Ultimate", path)
	}
	out := []Vulnerability{}
	for _, v := range res.Project.Vulnerabilities.Nodes {
		out = append(out, Vulnerability{ID: v.ID, Severity: strings.ToLower(v.Severity), Summary: v.Title, URL: v.WebURL})
	}
	return out, nil
}
