package trackers

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

type GitHub struct {
	cfg     *config.Tracker
	api     string
	headers map[string]string
	mu      sync.Mutex
	login   string
}

type ghIssue struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	PullRequest *struct {
	} `json:"pull_request"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	Comments   int `json:"comments"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// GitHubToken: GITHUB_TOKEN, else the one the gh CLI is logged in with.
func GitHubToken(cfg *config.Config) string {
	if t := cfg.Get("GITHUB_TOKEN"); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func NewGitHub(t *config.Tracker, token string) (*GitHub, error) {
	if token == "" {
		return nil, missing("GITHUB_TOKEN", "або увійдіть: gh auth login")
	}
	api := "https://api.github.com"
	if t.APIURL != "" {
		api = t.APIURL
	}
	return &GitHub{cfg: t, api: api, headers: map[string]string{"Authorization": "Bearer " + token, "X-GitHub-Api-Version": "2022-11-28"}}, nil
}

func (g *GitHub) ID() string              { return g.cfg.ID }
func (g *GitHub) Name() string            { return "GitHub" }
func (g *GitHub) Config() *config.Tracker { return g.cfg }

func (g *GitHub) Host() string {
	if g.cfg.APIURL == "" {
		return "github.com"
	}
	u, _ := url.Parse(g.cfg.APIURL)
	return strings.TrimPrefix(u.Hostname(), "api.")
}

func (g *GitHub) do(ctx context.Context, method, path string, body, out any) error {
	return call(ctx, method, g.api+path, g.headers, body, out, func(status int, _ string) string {
		switch status {
		case 401:
			return "GitHub не прийняв токен (401): перевірте GITHUB_TOKEN або gh auth login"
		case 403:
			return "GitHub заборонив дію (403): токену бракує прав (repo) або вичерпано ліміт запитів"
		}
		return ""
	})
}

func (g *GitHub) Me(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.login != "" {
		return g.login, nil
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := g.do(ctx, "GET", "/user", nil, &u); err != nil {
		return "", err
	}
	g.login = u.Login
	return g.login, nil
}

func (g *GitHub) FetchMine(ctx context.Context) ([]core.Task, error) {
	seen := map[string]bool{}
	var tasks []core.Task
	add := func(repo string, is ghIssue) {
		if is.PullRequest != nil {
			return
		}
		t := g.toTask(repo, is)
		if seen[t.ID] {
			return
		}
		seen[t.ID] = true
		if is.Comments > 0 {
			var cs []struct {
				Body string `json:"body"`
				User struct {
					Login string `json:"login"`
				} `json:"user"`
			}
			if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, is.Number), nil, &cs); err == nil {
				for _, c := range cs {
					t.Comments = append(t.Comments, core.Comment{Author: c.User.Login, Body: c.Body})
				}
			}
		}
		tasks = append(tasks, t)
	}
	if g.cfg.AllProjects {
		q := url.Values{"filter": {"assigned"}, "state": {"open"}}
		if g.cfg.Trigger != "" {
			q.Set("labels", g.cfg.Trigger)
		}
		issues, err := allPages(q, func(query string, out *[]ghIssue) error { return g.do(ctx, "GET", "/issues?"+query, nil, out) })
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is.Repository != nil && g.cfg.Projects[is.Repository.FullName] == nil {
				add(is.Repository.FullName, is)
			}
		}
	}
	assignee := g.cfg.Assignee
	if assignee == "" && g.cfg.AllProjects && g.cfg.Trigger == "" {
		assignee = "me"
	}
	if strings.EqualFold(assignee, "me") {
		me, err := g.Me(ctx)
		if err != nil {
			return nil, err
		}
		assignee = me
	}
	for repo := range g.cfg.Projects {
		q := url.Values{"state": {"open"}}
		if g.cfg.Trigger != "" {
			q.Set("labels", g.cfg.Trigger)
		}
		if assignee != "" {
			q.Set("assignee", assignee)
		}
		issues, err := allPages(q, func(query string, out *[]ghIssue) error {
			return g.do(ctx, "GET", "/repos/"+repo+"/issues?"+query, nil, out)
		})
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			add(repo, is)
		}
	}
	return tasks, nil
}

func (g *GitHub) toTask(repo string, is ghIssue) core.Task {
	labels := []string{}
	for _, l := range is.Labels {
		labels = append(labels, l.Name)
	}
	assignees := []string{}
	for _, a := range is.Assignees {
		assignees = append(assignees, a.Login)
	}
	meta := ""
	if len(labels) > 0 {
		meta = "Labels: " + strings.Join(labels, ", ")
	}
	author := ""
	if is.User != nil {
		author = is.User.Login
	}
	return core.Task{
		ID:          fmt.Sprintf("%s#%d", repo, is.Number),
		Slug:        fmt.Sprintf("%s-%d", repo[strings.Index(repo, "/")+1:], is.Number),
		TrackerID:   g.cfg.ID,
		Project:     repo,
		Title:       is.Title,
		Description: is.Body,
		URL:         is.HTMLURL,
		Meta:        meta,
		Labels:      labels,
		Components:  []string{},
		Comments:    []core.Comment{},
		ClosingRef:  fmt.Sprintf("Closes %s#%d", repo, is.Number),
		Assignees:   assignees,
		Author:      author,
	}
}

func ghIssuePath(t core.Task) string {
	return fmt.Sprintf("/repos/%s/issues/%s", t.Project, t.ID[strings.LastIndex(t.ID, "#")+1:])
}

func (g *GitHub) AssignToMe(ctx context.Context, t core.Task) error {
	me, err := g.Me(ctx)
	if err != nil {
		return err
	}
	return g.do(ctx, "POST", ghIssuePath(t)+"/assignees", map[string]any{"assignees": []string{me}}, nil)
}

func (g *GitHub) Comment(ctx context.Context, t core.Task, text string) error {
	return g.do(ctx, "POST", ghIssuePath(t)+"/comments", map[string]any{"body": text}, nil)
}

func (g *GitHub) IssueState(ctx context.Context, t core.Task) (string, error) {
	var is ghIssue
	if err := g.do(ctx, "GET", ghIssuePath(t), nil, &is); err != nil {
		if isStatus(err, 404) || isStatus(err, 410) {
			return "deleted", nil
		}
		return "", err
	}
	if is.State == "closed" {
		return "closed", nil
	}
	return "open", nil
}

// UserExists: GitHub knows the login.
func (g *GitHub) UserExists(ctx context.Context, login string) (bool, error) {
	err := g.do(ctx, "GET", "/users/"+url.PathEscape(login), nil, nil)
	if isStatus(err, 404) {
		return false, nil
	}
	return err == nil, err
}

// Move: GitHub issues have no statuses beyond open/closed; after a merge the issue is closed.
func (g *GitHub) Move(ctx context.Context, t core.Task, moment string) (string, error) {
	if moment != AtMerged {
		return "", nil
	}
	if st, err := g.IssueState(ctx, t); err != nil || st != "open" {
		return "", nil
	}
	err := g.do(ctx, "PATCH", ghIssuePath(t), map[string]any{"state": "closed"}, nil)
	// The merge closes the issue by itself when the MR says «Closes #N»: GitHub may do it between our look and
	// our PATCH and answer 422 «closed by another request».
	if isStatus(err, 422) {
		if st, e := g.IssueState(ctx, t); e == nil && st != "open" {
			return "", nil
		}
	}
	return "", err
}

// --- pull requests

var prURL = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)`)

type ghPull struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	HTMLURL  string  `json:"html_url"`
	MergedAt *string `json:"merged_at"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	// Mergeable: nil while GitHub is still working it out.
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p ghPull) state() string {
	if p.MergedAt != nil {
		return "merged"
	}
	if p.State == "closed" {
		return "closed"
	}
	return "open"
}

func (g *GitHub) FindReview(ctx context.Context, repo, branch string) (*Review, error) {
	owner := repo[:strings.Index(repo, "/")]
	q := url.Values{"head": {owner + ":" + branch}, "state": {"all"}, "per_page": {"1"}}
	var pulls []ghPull
	if err := g.do(ctx, "GET", "/repos/"+repo+"/pulls?"+q.Encode(), nil, &pulls); err != nil {
		return nil, err
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	return &Review{URL: pulls[0].HTMLURL, Number: pulls[0].Number, State: pulls[0].state()}, nil
}

func (g *GitHub) pull(ctx context.Context, reviewURL string) (string, *ghPull, error) {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return "", nil, fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	path := "/repos/" + m[1] + "/pulls/" + m[2]
	var p ghPull
	if err := g.do(ctx, "GET", path, nil, &p); err != nil {
		return "", nil, err
	}
	return path, &p, nil
}

func (g *GitHub) ReviewState(ctx context.Context, reviewURL string) (string, error) {
	_, p, err := g.pull(ctx, reviewURL)
	if err != nil {
		return "", err
	}
	return p.state(), nil
}

func (g *GitHub) ReadReview(ctx context.Context, reviewURL string) (string, string, error) {
	_, p, err := g.pull(ctx, reviewURL)
	if err != nil {
		return "", "", err
	}
	return p.Title, p.Body, nil
}

func (g *GitHub) DescribeReview(ctx context.Context, reviewURL, title, description string) error {
	m := prURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("не схоже на PR GitHub: %s", reviewURL)
	}
	return g.do(ctx, "PATCH", "/repos/"+m[1]+"/pulls/"+m[2], map[string]any{"title": title, "body": description}, nil)
}

// --- new issues

func (g *GitHub) Create(ctx context.Context, n NewIssue) (core.Task, error) {
	me, err := g.Me(ctx)
	if err != nil {
		return core.Task{}, err
	}
	body := map[string]any{"title": n.Title, "body": n.Description, "assignees": []string{me}, "labels": newLabels(g.cfg, n.Bug)}
	var is ghIssue
	if err := g.do(ctx, "POST", "/repos/"+n.Project+"/issues", body, &is); err != nil {
		return core.Task{}, err
	}
	return g.toTask(n.Project, is), nil
}

// UploadRepo keeps the images pasted into new issues and the agents' screenshots: GitHub's API cannot attach
// a file to an issue. One repository per owner (<owner>/mako-uploads), so the projects' own repositories
// get no side branch that GitHub keeps offering to turn into a pull request.
const UploadRepo = "mako-uploads"

// Upload commits the image to <owner>/mako-uploads (made the first time, as private as the project) and links it.
func (g *GitHub) Upload(ctx context.Context, repo, name string, data []byte) (string, error) {
	var project struct {
		Name    string `json:"name"`
		Private bool   `json:"private"`
		Owner   struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if err := g.do(ctx, "GET", "/repos/"+repo, nil, &project); err != nil {
		return "", err
	}
	owner, projectName, _ := strings.Cut(repo, "/")
	if project.Owner.Login != "" {
		owner = project.Owner.Login
	}
	if project.Name != "" {
		projectName = project.Name
	}
	target := owner + "/" + UploadRepo
	branch, err := g.uploadRepo(ctx, target, project.Owner.Type == "Organization", project.Private)
	if err != nil {
		return "", err
	}
	path := uploadPath(projectName, name)
	body := map[string]any{"message": "Mako: " + repo + ": " + name, "content": base64.StdEncoding.EncodeToString(data), "branch": branch}
	if err := g.do(ctx, "PUT", "/repos/"+target+"/contents/"+path, body, nil); err != nil {
		return "", err
	}
	return fmt.Sprintf("![%s](https://%s/%s/blob/%s/%s?raw=true)", name, g.Host(), target, branch, path), nil
}

type ghUploadRepo struct {
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// uploadRepo finds or makes the owner's images repository and answers the branch to commit to.
// A private project's screenshots never go to a public one.
func (g *GitHub) uploadRepo(ctx context.Context, target string, org, private bool) (string, error) {
	created := false
	for {
		var r ghUploadRepo
		err := g.do(ctx, "GET", "/repos/"+target, nil, &r)
		if err == nil {
			if private && !r.Private {
				return "", fmt.Errorf("%s публічний, а проєкт приватний: скриншоти туди не вантажу — зробіть %s приватним", target, target)
			}
			if r.DefaultBranch == "" {
				return "main", nil
			}
			return r.DefaultBranch, nil
		}
		if !isStatus(err, 404) || created {
			return "", err
		}
		created = true
		branch, err := g.createUploadRepo(ctx, target, org, private)
		if err != nil || branch != "" {
			return branch, err
		}
		// made meanwhile: look at it again
	}
}

// createUploadRepo answers the new repository's branch, or "" when it appeared meanwhile.
func (g *GitHub) createUploadRepo(ctx context.Context, target string, org, private bool) (string, error) {
	owner, name, _ := strings.Cut(target, "/")
	cantCreate := func(err error) error {
		return fmt.Errorf("не вдалося створити %s: створіть %s вручну або дайте токену право створювати репозиторії (%v)", target, target, err)
	}
	path := "/orgs/" + owner + "/repos"
	if !org {
		me, err := g.Me(ctx)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(me, owner) {
			return "", cantCreate(fmt.Errorf("токен належить %s, а не %s", me, owner))
		}
		path = "/user/repos"
	}
	body := map[string]any{"name": name, "private": private, "auto_init": true,
		"description": "Images Mako attaches to issues and merge requests"}
	var r ghUploadRepo
	err := g.do(ctx, "POST", path, body, &r)
	switch {
	case isStatus(err, 422): // made meanwhile
		return "", nil
	case err != nil:
		return "", cantCreate(err)
	case r.DefaultBranch == "":
		return "main", nil
	}
	return r.DefaultBranch, nil
}
