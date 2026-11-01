package trackers

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

type GitLab struct {
	cfg     *config.Tracker
	api     string // https://gitlab.example.com/api/v4
	headers map[string]string

	mu       sync.Mutex
	me       *glUser
	statuses []string
	// sizes: the lines an MR to review adds and removes, by its path and head commit.
	sizes map[string][2]int
}

type glUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type glIssue struct {
	ID          int      `json:"id"`
	IID         int      `json:"iid"`
	State       string   `json:"state"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	WebURL      string   `json:"web_url"`
	Labels      []string `json:"labels"`
	IssueType   string   `json:"issue_type"`
	Assignees   []glUser `json:"assignees"`
	Author      glUser   `json:"author"`
	NotesCount  int      `json:"user_notes_count"`
	References  struct {
		Full string `json:"full"`
	} `json:"references"`
}

func NewGitLab(t *config.Tracker, token string) (*GitLab, error) {
	if token == "" {
		return nil, missing("GITLAB_TOKEN", "personal access token зі scope api")
	}
	return &GitLab{cfg: t, api: strings.TrimRight(t.BaseURL, "/") + "/api/v4", headers: map[string]string{"PRIVATE-TOKEN": token}}, nil
}

func (g *GitLab) ID() string              { return g.cfg.ID }
func (g *GitLab) Name() string            { return "GitLab" }
func (g *GitLab) Config() *config.Tracker { return g.cfg }

func (g *GitLab) Host() string {
	u, _ := url.Parse(g.cfg.BaseURL)
	return u.Hostname()
}

func (g *GitLab) do(ctx context.Context, method, path string, body, out any) error {
	tokens := strings.TrimSuffix(g.api, "/api/v4") + "/-/user_settings/personal_access_tokens"
	return call(ctx, method, g.api+path, g.headers, body, out, func(status int, text string) string {
		switch {
		case status == 401:
			return "GitLab не прийняв токен (401): він недійсний або прострочений. Створіть новий: " + tokens
		case status == 403 && strings.Contains(text, "insufficient_scope"):
			return "Токену GitLab бракує прав: потрібен scope «api». Створіть токен зі scope api: " + tokens
		case status == 403:
			return "GitLab заборонив дію (403): у вашого акаунта немає потрібних прав у цьому проєкті"
		}
		return ""
	})
}

func project(path string) string { return "/projects/" + url.PathEscape(path) }

func (g *GitLab) Me(ctx context.Context) (string, error) {
	u, err := g.user(ctx)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// UserExists: GitLab knows the username. Its issue search takes an unknown one quietly and finds nothing.
func (g *GitLab) UserExists(ctx context.Context, login string) (bool, error) {
	var users []glUser
	if err := g.do(ctx, "GET", "/users?"+url.Values{"username": {login}}.Encode(), nil, &users); err != nil {
		return false, err
	}
	return len(users) > 0, nil
}

func (g *GitLab) user(ctx context.Context) (*glUser, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.me != nil {
		return g.me, nil
	}
	var u glUser
	if err := g.do(ctx, "GET", "/user", nil, &u); err != nil {
		return nil, err
	}
	g.me = &u
	return g.me, nil
}

// FetchMine: everything assigned to me (any project) when allProjects is on, and the listed projects'
// issues by their filters: the trigger label, the assignee («me» without a label and with allProjects).
func (g *GitLab) FetchMine(ctx context.Context) ([]core.Task, error) {
	seen := map[string]bool{}
	var tasks []core.Task
	add := func(projectPath string, is glIssue) error {
		t := g.toTask(projectPath, is)
		if seen[t.ID] {
			return nil
		}
		seen[t.ID] = true
		if is.NotesCount > 0 {
			var notes []struct {
				System bool   `json:"system"`
				Body   string `json:"body"`
				Author struct {
					Name string `json:"name"`
				} `json:"author"`
			}
			if err := g.do(ctx, "GET", fmt.Sprintf("%s/issues/%d/notes?sort=asc&per_page=100", project(projectPath), is.IID), nil, &notes); err == nil {
				for _, n := range notes {
					if !n.System {
						t.Comments = append(t.Comments, core.Comment{Author: n.Author.Name, Body: n.Body})
					}
				}
			}
		}
		tasks = append(tasks, t)
		return nil
	}
	if g.cfg.AllProjects {
		q := url.Values{"scope": {"assigned_to_me"}, "state": {"opened"}}
		if g.cfg.Trigger != "" {
			q.Set("labels", g.cfg.Trigger)
		}
		issues, err := allPages(q, func(query string, out *[]glIssue) error { return g.do(ctx, "GET", "/issues?"+query, nil, out) })
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if p := issueProject(is); p != "" && g.cfg.Projects[p] == nil {
				_ = add(p, is)
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
	for key := range g.cfg.Projects {
		q := url.Values{"state": {"opened"}}
		if g.cfg.Trigger != "" {
			q.Set("labels", g.cfg.Trigger)
		}
		if assignee != "" {
			q.Set("assignee_username", assignee)
		}
		issues, err := allPages(q, func(query string, out *[]glIssue) error {
			return g.do(ctx, "GET", project(key)+"/issues?"+query, nil, out)
		})
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			_ = add(key, is)
		}
	}
	return tasks, nil
}

var issueURL = regexp.MustCompile(`^https?://[^/]+/(.+?)/-/(?:issues|work_items)/`)

// issueProject: «group/sub/app» from an instance-wide list item.
func issueProject(is glIssue) string {
	if i := strings.LastIndex(is.References.Full, "#"); i > 0 {
		return is.References.Full[:i]
	}
	if m := issueURL.FindStringSubmatch(is.WebURL); m != nil {
		return m[1]
	}
	return ""
}

func (g *GitLab) toTask(projectPath string, is glIssue) core.Task {
	meta := []string{}
	if is.IssueType != "" {
		meta = append(meta, "Type: "+strings.ToUpper(is.IssueType[:1])+is.IssueType[1:])
	}
	if len(is.Labels) > 0 {
		meta = append(meta, "Labels: "+strings.Join(is.Labels, ", "))
	}
	assignees := []string{}
	for _, a := range is.Assignees {
		assignees = append(assignees, a.Username)
	}
	name := projectPath[strings.LastIndex(projectPath, "/")+1:]
	return core.Task{
		ID:          fmt.Sprintf("%s#%d", projectPath, is.IID),
		Slug:        fmt.Sprintf("%s-%d", name, is.IID),
		TrackerID:   g.cfg.ID,
		Project:     projectPath,
		Title:       is.Title,
		Description: is.Description,
		URL:         is.WebURL,
		Meta:        strings.Join(meta, " | "),
		Labels:      orEmpty(is.Labels),
		Components:  []string{},
		Comments:    []core.Comment{},
		ClosingRef:  fmt.Sprintf("Closes %s#%d", projectPath, is.IID),
		NativeID:    strconv.Itoa(is.ID),
		Assignees:   assignees,
		Author:      is.Author.Username,
	}
}

func issuePath(t core.Task) string {
	return fmt.Sprintf("%s/issues/%s", project(t.Project), t.ID[strings.LastIndex(t.ID, "#")+1:])
}

// AssignToMe adds me to the assignees instead of replacing them; me first, for GitLab Free that keeps one.
func (g *GitLab) AssignToMe(ctx context.Context, t core.Task) error {
	me, err := g.user(ctx)
	if err != nil {
		return err
	}
	var is glIssue
	if err := g.do(ctx, "GET", issuePath(t), nil, &is); err != nil {
		return err
	}
	ids := []int{me.ID}
	for _, a := range is.Assignees {
		if a.ID != me.ID {
			ids = append(ids, a.ID)
		}
	}
	return g.do(ctx, "PUT", issuePath(t), map[string]any{"assignee_ids": ids}, nil)
}

func (g *GitLab) Comment(ctx context.Context, t core.Task, text string) error {
	return g.do(ctx, "POST", issuePath(t)+"/notes", map[string]any{"body": text}, nil)
}

func (g *GitLab) IssueState(ctx context.Context, t core.Task) (string, error) {
	var is glIssue
	if err := g.do(ctx, "GET", issuePath(t), nil, &is); err != nil {
		if isStatus(err, 404) {
			return "deleted", nil
		}
		return "", err
	}
	if is.State == "closed" {
		return "closed", nil
	}
	return "open", nil
}

// Move sets the work item status for a moment: the configured one, or for «start» the project's In progress
// and for «merged» its Done, when the project has such statuses. After a merge the issue is closed too.
func (g *GitLab) Move(ctx context.Context, t core.Task, moment string) (string, error) {
	tr := g.cfg.Transitions
	var name string
	switch moment {
	case AtStart:
		name = pick(tr.OnStart, func() string {
			return g.guess(ctx, t, regexp.MustCompile(`(?i)progress|в роботі|doing`), "in progress")
		})
	case AtReview:
		name = pick(tr.OnSuccess, nil)
	case AtFailed:
		name = pick(tr.OnFailure, nil)
	case AtMerged:
		name = pick(tr.OnMerged, func() string {
			return g.guess(ctx, t, regexp.MustCompile(`(?i)^done$|^closed$|completed|готово|виконано`), "done")
		})
	}
	moved := ""
	if name != "" {
		if err := g.setStatus(ctx, t, name); err != nil {
			return "", err
		}
		moved = name
	}
	if moment == AtMerged {
		if st, err := g.IssueState(ctx, t); err == nil && st == "open" {
			if err := g.do(ctx, "PUT", issuePath(t), map[string]any{"state_event": "close"}, nil); err != nil {
				return moved, err
			}
		}
	}
	return moved, nil
}

// pick: the configured name ("" means leave it), or the guess when nothing is configured.
func pick(set *string, guess func() string) string {
	if set != nil {
		return *set
	}
	if guess != nil {
		return guess()
	}
	return ""
}

func (g *GitLab) guess(ctx context.Context, t core.Task, pattern *regexp.Regexp, exact string) string {
	g.mu.Lock()
	known := g.statuses
	g.mu.Unlock()
	if known == nil {
		known, _ = g.listStatuses(ctx, t.Project)
		g.mu.Lock()
		g.statuses = orEmpty(known)
		g.mu.Unlock()
	}
	for _, s := range known {
		if strings.EqualFold(s, exact) {
			return s
		}
	}
	for _, s := range known {
		if pattern.MatchString(s) {
			return s
		}
	}
	return ""
}

func (g *GitLab) listStatuses(ctx context.Context, projectPath string) ([]string, error) {
	var data struct {
		Project struct {
			WorkItemTypes struct {
				Nodes []struct {
					Name              string `json:"name"`
					WidgetDefinitions []struct {
						AllowedStatuses []struct {
							Name string `json:"name"`
						} `json:"allowedStatuses"`
					} `json:"widgetDefinitions"`
				} `json:"nodes"`
			} `json:"workItemTypes"`
		} `json:"project"`
	}
	err := g.graphql(ctx, `query($path: ID!) { project(fullPath: $path) { workItemTypes { nodes { name widgetDefinitions {
		type ... on WorkItemWidgetDefinitionStatus { allowedStatuses { name } } } } } } }`, map[string]any{"path": projectPath}, &data)
	if err != nil {
		return nil, err
	}
	for _, typ := range data.Project.WorkItemTypes.Nodes {
		if !strings.EqualFold(typ.Name, "issue") {
			continue
		}
		for _, w := range typ.WidgetDefinitions {
			if len(w.AllowedStatuses) > 0 {
				names := []string{}
				for _, s := range w.AllowedStatuses {
					names = append(names, s.Name)
				}
				return names, nil
			}
		}
	}
	return []string{}, nil
}

func (g *GitLab) setStatus(ctx context.Context, t core.Task, name string) error {
	id := t.NativeID
	if id == "" {
		var is glIssue
		if err := g.do(ctx, "GET", issuePath(t), nil, &is); err != nil {
			return err
		}
		id = strconv.Itoa(is.ID)
	}
	var data struct {
		WorkItemUpdate struct {
			Errors []string `json:"errors"`
		} `json:"workItemUpdate"`
	}
	err := g.graphql(ctx, `mutation($id: WorkItemID!, $name: String!) { workItemUpdate(input: { id: $id, statusWidget: { name: $name } }) { errors } }`,
		map[string]any{"id": "gid://gitlab/WorkItem/" + id, "name": name}, &data)
	if err != nil {
		return err
	}
	if len(data.WorkItemUpdate.Errors) > 0 {
		return fmt.Errorf("GitLab не змінив статус %s на «%s»: %s", t.ID, name, strings.Join(data.WorkItemUpdate.Errors, "; "))
	}
	return nil
}

func (g *GitLab) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	var res struct {
		Data   jsonRaw `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	endpoint := strings.TrimSuffix(g.api, "/api/v4") + "/api/graphql"
	if err := call(ctx, "POST", endpoint, g.headers, map[string]any{"query": query, "variables": vars}, &res, nil); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		msgs := []string{}
		for _, e := range res.Errors {
			msgs = append(msgs, e.Message)
		}
		msg := strings.Join(msgs, "; ")
		if regexp.MustCompile(`(?i)statusWidget|WorkItemWidgetDefinitionStatus|allowedStatuses|doesn't exist on type|not available`).MatchString(msg) {
			return fmt.Errorf("статуси задач на цьому GitLab недоступні (потрібні GitLab 17.11+ і Premium/Ultimate): %s", msg)
		}
		return fmt.Errorf("GitLab GraphQL: %s", msg)
	}
	return res.Data.decode(out)
}

// --- merge requests

var mrURL = regexp.MustCompile(`^https?://[^/]+/(.+?)/-/merge_requests/(\d+)`)

type glMR struct {
	IID          int    `json:"iid"`
	State        string `json:"state"`
	WebURL       string `json:"web_url"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	HasConflicts bool   `json:"has_conflicts"`
	MergeStatus  string `json:"detailed_merge_status"`
	HeadPipeline *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	} `json:"head_pipeline"`
}

func mrState(s string) string {
	switch s {
	case "merged":
		return "merged"
	case "closed", "locked":
		return "closed"
	}
	return "open"
}

func (g *GitLab) FindReview(ctx context.Context, projectPath, branch string) (*Review, error) {
	q := url.Values{"source_branch": {branch}, "state": {"all"}, "order_by": {"created_at"}, "sort": {"desc"}, "per_page": {"1"}}
	var mrs []glMR
	if err := g.do(ctx, "GET", project(projectPath)+"/merge_requests?"+q.Encode(), nil, &mrs); err != nil {
		return nil, err
	}
	if len(mrs) == 0 {
		return nil, nil
	}
	return &Review{URL: mrs[0].WebURL, Number: mrs[0].IID, State: mrState(mrs[0].State)}, nil
}

func (g *GitLab) mr(ctx context.Context, reviewURL string) (string, *glMR, error) {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return "", nil, fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	path := project(m[1]) + "/merge_requests/" + m[2]
	var mr glMR
	if err := g.do(ctx, "GET", path, nil, &mr); err != nil {
		return "", nil, err
	}
	return path, &mr, nil
}

func (g *GitLab) ReviewState(ctx context.Context, reviewURL string) (string, error) {
	_, mr, err := g.mr(ctx, reviewURL)
	if err != nil {
		return "", err
	}
	return mrState(mr.State), nil
}

func (g *GitLab) ReadReview(ctx context.Context, reviewURL string) (string, string, error) {
	_, mr, err := g.mr(ctx, reviewURL)
	if err != nil {
		return "", "", err
	}
	return mr.Title, mr.Description, nil
}

func (g *GitLab) DescribeReview(ctx context.Context, reviewURL, title, description string) error {
	m := mrURL.FindStringSubmatch(reviewURL)
	if m == nil {
		return fmt.Errorf("не схоже на MR GitLab: %s", reviewURL)
	}
	return g.do(ctx, "PUT", project(m[1])+"/merge_requests/"+m[2], map[string]any{"title": title, "description": description}, nil)
}

func (g *GitLab) OpenReview(ctx context.Context, projectPath, branch, base, title, description string) (*Review, error) {
	var mr glMR
	body := map[string]any{"source_branch": branch, "target_branch": base, "title": title, "description": description, "remove_source_branch": true}
	if err := g.do(ctx, "POST", project(projectPath)+"/merge_requests", body, &mr); err != nil {
		return nil, err
	}
	return &Review{URL: mr.WebURL, Number: mr.IID, State: mrState(mr.State)}, nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// --- new issues

// Create: a bug is a GitLab Issue, a task a GitLab Task, as TaskType reads them back.
func (g *GitLab) Create(ctx context.Context, n NewIssue) (core.Task, error) {
	me, err := g.user(ctx)
	if err != nil {
		return core.Task{}, err
	}
	kind := "task"
	if n.Bug {
		kind = "issue"
	}
	body := map[string]any{"title": n.Title, "description": n.Description, "assignee_ids": []int{me.ID}, "issue_type": kind,
		"labels": strings.Join(newLabels(g.cfg, false), ",")}
	var is glIssue
	if err := g.do(ctx, "POST", project(n.Project)+"/issues", body, &is); err != nil {
		return core.Task{}, err
	}
	return g.toTask(n.Project, is), nil
}

func (g *GitLab) Upload(ctx context.Context, projectPath, name string, data []byte) (string, error) {
	form, err := fileForm("file", name, data)
	if err != nil {
		return "", err
	}
	var up struct {
		Markdown string `json:"markdown"`
	}
	if err := g.do(ctx, "POST", project(projectPath)+"/uploads", form, &up); err != nil {
		return "", err
	}
	return up.Markdown, nil
}
