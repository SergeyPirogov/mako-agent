package trackers

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Check is one condition a task must meet for Mako's search to find it; OK false says why it does not.
type Check struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

// Explainer is a tracker that can say why a task by its link is or is not among the ones Mako finds:
// «Чому немає задачі?» on the board. Why returns nil and no error when the link is not this tracker's.
type Explainer interface {
	Why(ctx context.Context, link string) (*core.Task, []Check, error)
}

// sameHost: the link is on host ("gitlab.example.com"); a bare host without a scheme counts too.
func sameHost(link, host string) (*url.URL, bool) {
	if !strings.Contains(link, "://") {
		link = "https://" + link
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Hostname() == "" {
		return nil, false
	}
	return u, strings.EqualFold(strings.TrimPrefix(u.Hostname(), "www."), strings.TrimPrefix(host, "www."))
}

// issueFacts: what of a GitLab or GitHub issue decides whether FetchMine finds it.
type issueFacts struct {
	Project   string
	Open      bool
	Labels    []string
	Assignees []string
}

// searchChecks repeats FetchMine's filters for one issue of GitLab or GitHub, in the order a person fixes them.
func searchChecks(ctx context.Context, t Tracker, f issueFacts) []Check {
	cfg := t.Config()
	var checks []Check
	add := func(ok bool, yes, no string) {
		checks = append(checks, Check{OK: ok, Text: map[bool]string{true: yes, false: no}[ok]})
	}

	add(f.Open, "The task is open", "The task is closed: Mako only looks for open ones")

	listed := cfg.Projects[f.Project] != nil
	switch {
	case listed:
		add(true, "Project "+f.Project+" is in the tracker settings", "")
	case cfg.AllProjects:
		add(true, "Project "+f.Project+" is not in the settings, but \"My tasks from all projects\" is on", "")
	default:
		add(false, "", "Project "+f.Project+" is not in the tracker settings and \"My tasks from all projects\" is off: add the project in settings")
	}

	if cfg.Trigger != "" {
		has := false
		for _, l := range f.Labels {
			has = has || strings.EqualFold(l, cfg.Trigger)
		}
		add(has, "Has the label \""+cfg.Trigger+"\"", "Missing the label \""+cfg.Trigger+"\": Mako only takes tasks that have it. Add the label or turn it off in settings")
	}

	me, meErr := t.Me(ctx)
	assigned := func(who string) bool {
		for _, a := range f.Assignees {
			if strings.EqualFold(a, who) {
				return true
			}
		}
		return false
	}
	whom := func() string {
		if len(f.Assignees) == 0 {
			return "not assigned to anyone"
		}
		return "assigned to " + strings.Join(f.Assignees, ", ")
	}
	// Who the search wants the task assigned to: me in all projects, the configured assignee in listed ones.
	want := ""
	if listed {
		want = cfg.Assignee
		if want == "" && cfg.AllProjects && cfg.Trigger == "" {
			want = "me"
		}
	} else if cfg.AllProjects {
		want = "me"
	}
	if strings.EqualFold(want, "me") {
		want = me
		if meErr != nil {
			add(false, "", "Could not find out who you are in the tracker: "+meErr.Error())
			return checks
		}
	}
	if want != "" {
		add(assigned(want), "The task is assigned to "+want, "The task is "+whom()+", and Mako looks for tasks assigned to "+want)
	}
	if cfg.SkipOthersTasks && me != "" && len(f.Assignees) > 0 {
		add(assigned(me), "The task is not someone else's", "The task is "+whom()+", and \"Skip tasks assigned to others\" is on")
	}
	return checks
}

// --- GitHub

func (g *GitHub) Why(ctx context.Context, link string) (*core.Task, []Check, error) {
	u, ok := sameHost(link, g.Host())
	if !ok {
		return nil, nil, nil
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return nil, nil, fmt.Errorf("not a GitHub issue link: %s (expected …/owner/repo/issues/N)", link)
	}
	repo := parts[0] + "/" + parts[1]
	var is ghIssue
	if err := g.do(ctx, "GET", "/repos/"+repo+"/issues/"+parts[3], nil, &is); err != nil {
		if isStatus(err, 404) || isStatus(err, 410) {
			return nil, nil, fmt.Errorf("GitHub did not find %s#%s: the task was deleted or the token has no access to the repository", repo, parts[3])
		}
		return nil, nil, err
	}
	t := g.toTask(repo, is)
	if is.PullRequest != nil {
		return &t, []Check{{Text: "This is a pull request, not an issue: Mako only works on issues"}}, nil
	}
	return &t, searchChecks(ctx, g, issueFacts{Project: repo, Open: is.State == "open", Labels: t.Labels, Assignees: t.Assignees}), nil
}

// --- GitLab

func (g *GitLab) Why(ctx context.Context, link string) (*core.Task, []Check, error) {
	u, ok := sameHost(link, g.Host())
	if !ok {
		return nil, nil, nil
	}
	path := strings.Trim(u.Path, "/")
	i := strings.Index(path, "/-/")
	if i < 0 {
		return nil, nil, fmt.Errorf("not a GitLab issue link: %s (expected …/group/app/-/issues/N)", link)
	}
	projectPath, rest := path[:i], strings.Split(path[i+3:], "/")
	if len(rest) < 2 {
		return nil, nil, fmt.Errorf("not a GitLab issue link: %s", link)
	}
	if rest[0] == "merge_requests" {
		return &core.Task{ID: projectPath + "!" + rest[1], URL: link, Project: projectPath, TrackerID: g.cfg.ID},
			[]Check{{Text: "This is a merge request, not an issue: Mako only works on issues"}}, nil
	}
	if rest[0] != "issues" && rest[0] != "work_items" {
		return nil, nil, fmt.Errorf("not a GitLab issue link: %s (expected …/-/issues/N)", link)
	}
	var is glIssue
	if err := g.do(ctx, "GET", project(projectPath)+"/issues/"+rest[1], nil, &is); err != nil {
		if isStatus(err, 404) {
			return nil, nil, fmt.Errorf("GitLab did not find %s#%s: the task was deleted or the token has no access to the project", projectPath, rest[1])
		}
		return nil, nil, err
	}
	t := g.toTask(projectPath, is)
	return &t, searchChecks(ctx, g, issueFacts{Project: projectPath, Open: is.State == "opened", Labels: t.Labels, Assignees: t.Assignees}), nil
}

// --- Jira

func (j *Jira) Why(ctx context.Context, link string) (*core.Task, []Check, error) {
	host := ""
	if b, err := url.Parse(j.base); err == nil {
		host = b.Hostname()
	}
	u, ok := sameHost(link, host)
	if !ok {
		return nil, nil, nil
	}
	key := u.Query().Get("selectedIssue")
	if i := strings.Index(u.Path, "/browse/"); i >= 0 {
		key = strings.Trim(u.Path[i+len("/browse/"):], "/")
	}
	if key == "" {
		return nil, nil, fmt.Errorf("not a Jira issue link: %s (expected …/browse/KEY-1)", link)
	}
	found, err := j.search(ctx, "key = "+key, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(found) == 0 {
		return nil, nil, fmt.Errorf("Jira did not find %s: the task was deleted or you have no access to it", key)
	}
	t := found[0]
	var is jiraIssue
	if err := j.do(ctx, "GET", "/rest/api/3/issue/"+key+"?fields=status", nil, &is); err != nil {
		return nil, nil, err
	}
	var checks []Check
	done := is.Fields.Status.StatusCategory.Key == "done"
	checks = append(checks, map[bool]Check{
		true:  {Text: "The task is already in the \"Done\" status"},
		false: {OK: true, Text: "The task is not closed"},
	}[done])

	jql := j.cfg.JQL
	if strings.TrimSpace(jql) == "" {
		jql = `assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC`
	}
	filter := jql
	if i := strings.Index(strings.ToUpper(filter), "ORDER BY"); i >= 0 {
		filter = filter[:i]
	}
	match, err := j.search(ctx, "key = "+key+" AND ("+strings.TrimSpace(filter)+")", 1)
	if err != nil {
		return &t, append(checks, Check{Text: "The JQL query failed: " + err.Error()}), nil
	}
	if len(match) == 0 {
		return &t, append(checks, Check{Text: "The task does not match the JQL query: " + jql + ". Most often it is not assigned to you"}), nil
	}
	checks = append(checks, Check{OK: true, Text: "The task matches the JQL query"})
	// FetchMine reads the first 50 of the search.
	all, err := j.search(ctx, jql, 50)
	if err == nil {
		in := false
		for _, x := range all {
			in = in || x.ID == key
		}
		checks = append(checks, map[bool]Check{
			true:  {OK: true, Text: "The task is among the first 50 the query returns"},
			false: {Text: "The query returns more than 50 tasks and this one is not among the first 50: narrow the JQL"},
		}[in])
	}
	if j.cfg.SkipOthersTasks && len(t.Assignees) > 0 {
		if me, err := j.Me(ctx); err == nil && t.Assignees[0] != me {
			checks = append(checks, Check{Text: "The task is assigned to someone else, and \"Skip other people's tasks\" is on"})
		}
	}
	return &t, checks, nil
}
