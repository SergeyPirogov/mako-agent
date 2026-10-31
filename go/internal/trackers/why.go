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

	add(f.Open, "Задача відкрита", "Задача закрита: Mako шукає лише відкриті")

	listed := cfg.Projects[f.Project] != nil
	switch {
	case listed:
		add(true, "Проєкт "+f.Project+" є в налаштуваннях трекера", "")
	case cfg.AllProjects:
		add(true, "Проєкту "+f.Project+" немає в налаштуваннях, але ввімкнено «Мої задачі з усіх проєктів»", "")
	default:
		add(false, "", "Проєкту "+f.Project+" немає в налаштуваннях трекера, а «Мої задачі з усіх проєктів» вимкнено: додайте проєкт у налаштуваннях")
	}

	if cfg.Trigger != "" {
		has := false
		for _, l := range f.Labels {
			has = has || strings.EqualFold(l, cfg.Trigger)
		}
		add(has, "Є мітка «"+cfg.Trigger+"»", "Немає мітки «"+cfg.Trigger+"»: Mako бере лише задачі з нею. Додайте мітку або вимкніть її в налаштуваннях")
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
			return "ні на кого не призначена"
		}
		return "призначена на " + strings.Join(f.Assignees, ", ")
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
			add(false, "", "Не вдалося дізнатися, хто ви в трекері: "+meErr.Error())
			return checks
		}
	}
	if want != "" {
		add(assigned(want), "Задача призначена на "+want, "Задача "+whom()+", а Mako шукає задачі, призначені на "+want)
	}
	if cfg.SkipOthersTasks && me != "" && len(f.Assignees) > 0 {
		add(assigned(me), "Задача не чужа", "Задача "+whom()+", а ввімкнено «Пропускати чужі задачі»")
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
		return nil, nil, fmt.Errorf("не схоже на посилання на issue GitHub: %s (потрібно …/owner/repo/issues/N)", link)
	}
	repo := parts[0] + "/" + parts[1]
	var is ghIssue
	if err := g.do(ctx, "GET", "/repos/"+repo+"/issues/"+parts[3], nil, &is); err != nil {
		if isStatus(err, 404) || isStatus(err, 410) {
			return nil, nil, fmt.Errorf("GitHub не знайшов %s#%s: задачу видалено або токен не має доступу до репозиторію", repo, parts[3])
		}
		return nil, nil, err
	}
	t := g.toTask(repo, is)
	if is.PullRequest != nil {
		return &t, []Check{{Text: "Це pull request, а не issue: Mako бере в роботу лише issues"}}, nil
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
		return nil, nil, fmt.Errorf("не схоже на посилання на задачу GitLab: %s (потрібно …/group/app/-/issues/N)", link)
	}
	projectPath, rest := path[:i], strings.Split(path[i+3:], "/")
	if len(rest) < 2 {
		return nil, nil, fmt.Errorf("не схоже на посилання на задачу GitLab: %s", link)
	}
	if rest[0] == "merge_requests" {
		return &core.Task{ID: projectPath + "!" + rest[1], URL: link, Project: projectPath, TrackerID: g.cfg.ID},
			[]Check{{Text: "Це merge request, а не задача: Mako бере в роботу лише issues"}}, nil
	}
	if rest[0] != "issues" && rest[0] != "work_items" {
		return nil, nil, fmt.Errorf("не схоже на посилання на задачу GitLab: %s (потрібно …/-/issues/N)", link)
	}
	var is glIssue
	if err := g.do(ctx, "GET", project(projectPath)+"/issues/"+rest[1], nil, &is); err != nil {
		if isStatus(err, 404) {
			return nil, nil, fmt.Errorf("GitLab не знайшов %s#%s: задачу видалено або токен не має доступу до проєкту", projectPath, rest[1])
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
		return nil, nil, fmt.Errorf("не схоже на посилання на задачу Jira: %s (потрібно …/browse/KEY-1)", link)
	}
	found, err := j.search(ctx, "key = "+key, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(found) == 0 {
		return nil, nil, fmt.Errorf("Jira не знайшла %s: задачу видалено або у вас немає до неї доступу", key)
	}
	t := found[0]
	var is jiraIssue
	if err := j.do(ctx, "GET", "/rest/api/3/issue/"+key+"?fields=status", nil, &is); err != nil {
		return nil, nil, err
	}
	var checks []Check
	done := is.Fields.Status.StatusCategory.Key == "done"
	checks = append(checks, map[bool]Check{
		true:  {Text: "Задача вже в статусі «Done»"},
		false: {OK: true, Text: "Задача не закрита"},
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
		return &t, append(checks, Check{Text: "Запит JQL не виконався: " + err.Error()}), nil
	}
	if len(match) == 0 {
		return &t, append(checks, Check{Text: "Задача не підходить під запит JQL: " + jql + ". Найчастіше вона не призначена на вас"}), nil
	}
	checks = append(checks, Check{OK: true, Text: "Задача підходить під запит JQL"})
	// FetchMine reads the first 50 of the search.
	all, err := j.search(ctx, jql, 50)
	if err == nil {
		in := false
		for _, x := range all {
			in = in || x.ID == key
		}
		checks = append(checks, map[bool]Check{
			true:  {OK: true, Text: "Задача серед перших 50, що знаходить запит"},
			false: {Text: "Запит знаходить понад 50 задач, а ця не серед перших 50: звузьте JQL"},
		}[in])
	}
	if j.cfg.SkipOthersTasks && len(t.Assignees) > 0 {
		if me, err := j.Me(ctx); err == nil && t.Assignees[0] != me {
			checks = append(checks, Check{Text: "Задача призначена на іншу людину, а ввімкнено «Пропускати чужі задачі»"})
		}
	}
	return &t, checks, nil
}
