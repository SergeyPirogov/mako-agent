package trackers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Jira Cloud: tasks by JQL, statuses by workflow transitions, comments in Atlassian's document format.
type Jira struct {
	cfg       *config.Tracker
	base      string
	headers   map[string]string
	mu        sync.Mutex
	accountID string
	// comments: the long issues' comments as the last FetchMine read them, by key.
	comments map[string]commentMemo
}

func NewJira(t *config.Tracker, email, token string) (*Jira, error) {
	if t.BaseURL == "" {
		return nil, fmt.Errorf("no Jira URL: set it in settings or JIRA_BASE_URL in .env")
	}
	if email == "" || token == "" {
		return nil, missing("JIRA_EMAIL and JIRA_API_TOKEN", "token: id.atlassian.com → Security → API tokens")
	}
	auth := base64.StdEncoding.EncodeToString([]byte(email + ":" + token))
	return &Jira{cfg: t, base: t.BaseURL, headers: map[string]string{"Authorization": "Basic " + auth}}, nil
}

func (j *Jira) ID() string              { return j.cfg.ID }
func (j *Jira) Name() string            { return "Jira" }
func (j *Jira) Config() *config.Tracker { return j.cfg }

func (j *Jira) do(ctx context.Context, method, path string, body, out any) error {
	return call(ctx, method, j.base+path, j.headers, body, out, func(status int, _ string) string {
		switch status {
		case 401:
			return "Jira rejected the credentials (401): check JIRA_EMAIL and the API token (id.atlassian.com → Security → API tokens)"
		case 403:
			return "Jira forbade the action (403): your account lacks the required permissions in this project"
		}
		return ""
	})
}

func (j *Jira) Me(ctx context.Context) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.accountID != "" {
		return j.accountID, nil
	}
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := j.do(ctx, "GET", "/rest/api/3/myself", nil, &me); err != nil {
		return "", err
	}
	j.accountID = me.AccountID
	return j.accountID, nil
}

type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Updated     string          `json:"updated"`
		Description json.RawMessage `json:"description"`
		IssueType   struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority *struct {
			Name string `json:"name"`
		} `json:"priority"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Labels     []string `json:"labels"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
		Assignee *struct {
			AccountID string `json:"accountId"`
		} `json:"assignee"`
		Reporter   *jiraUser    `json:"reporter"`
		Comment    jiraComments `json:"comment"`
		Status     jiraStatus   `json:"status"`
		IssueLinks []struct {
			Type struct {
				Inward string `json:"inward"`
			} `json:"type"`
			InwardIssue *struct {
				Key    string `json:"key"`
				Fields struct {
					Status jiraStatus `json:"status"`
				} `json:"fields"`
			} `json:"inwardIssue"`
		} `json:"issuelinks"`
	} `json:"fields"`
}

type jiraStatus struct {
	StatusCategory struct {
		Key string `json:"key"`
	} `json:"statusCategory"`
}

// blockedBy: the issues linked to this one as blocking it («is blocked by») that are not done yet.
func (is jiraIssue) blockedBy() []string {
	var keys []string
	for _, l := range is.Fields.IssueLinks {
		if in := l.InwardIssue; in != nil && strings.EqualFold(l.Type.Inward, "is blocked by") && in.Fields.Status.StatusCategory.Key != "done" {
			keys = append(keys, in.Key)
		}
	}
	return keys
}

// jiraComment: one comment, its text in Atlassian's document format.
type jiraComment struct {
	Body   json.RawMessage `json:"body"`
	Author jiraUser        `json:"author"`
}

// jiraComments: a page of an issue's comments; Total is how many the issue has, the page may hold fewer.
type jiraComments struct {
	Comments []jiraComment `json:"comments"`
	Total    int           `json:"total"`
}

// moreComments reads the comments that did not fit the search's embedded page: the issue's comment list, page by
// page by startAt, in the order they were written.
func (j *Jira) moreComments(ctx context.Context, key string, have []jiraComment, total int) ([]jiraComment, error) {
	all := slices.Clone(have)
	for page := 0; len(all) < total && page < maxPages; page++ {
		var res jiraComments
		q := url.Values{"startAt": {strconv.Itoa(len(all))}, "maxResults": {strconv.Itoa(perPage)}, "orderBy": {"created"}}
		if err := j.do(ctx, "GET", "/rest/api/3/issue/"+key+"/comment?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		if len(res.Comments) == 0 {
			break
		}
		all = append(all, res.Comments...)
	}
	return all, nil
}

// jiraUser: an issue's reporter or a comment's author.
type jiraUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	// AccountType: "atlassian" for the site's own users; "customer" for a service desk's portal customers, "app".
	AccountType string `json:"accountType"`
}

// member: the user is the project's: me, the issue's assignee, or one of the site's own users; a service desk's
// customer or an app is an outsider.
func (u *jiraUser) member(me string, assignees []string) bool {
	return byProject(u.AccountID, me, assignees) || u.AccountType == "atlassian"
}

// jiraMax: as many issues as FetchMine reads, page by page, like GitHub's and GitLab's lists.
const jiraMax = ListMax

// jiraFields: what a task is made of.
var jiraFields = []string{"summary", "updated", "description", "issuetype", "priority", "project", "comment", "labels", "components", "assignee", "reporter", "issuelinks"}

// pages reads up to max issues of a query with the given fields, a page after a page by nextPageToken, and stops
// after the page for which done says so (nil: reads to the end).
func (j *Jira) pages(ctx context.Context, jql string, max int, fields []string, done func([]jiraIssue) bool) ([]jiraIssue, error) {
	var issues []jiraIssue
	token := ""
	for len(issues) < max {
		var res struct {
			Issues        []jiraIssue `json:"issues"`
			NextPageToken string      `json:"nextPageToken"`
			IsLast        bool        `json:"isLast"`
		}
		body := map[string]any{"jql": jql, "maxResults": min(max-len(issues), perPage), "fields": fields}
		if token != "" {
			body["nextPageToken"] = token
		}
		if err := j.do(ctx, "POST", "/rest/api/3/search/jql", body, &res); err != nil {
			return nil, err
		}
		issues = append(issues, res.Issues...)
		if (done != nil && done(res.Issues)) || res.IsLast || res.NextPageToken == "" || res.NextPageToken == token || len(res.Issues) == 0 {
			break
		}
		token = res.NextPageToken
	}
	if len(issues) > max {
		issues = issues[:max]
	}
	return issues, nil
}

// contains: the query finds the issue among its first jiraMax; only the keys are read, and no page past the
// one that has it.
func (j *Jira) contains(ctx context.Context, jql, key string) (bool, error) {
	found := false
	_, err := j.pages(ctx, jql, jiraMax, []string{"summary"}, func(page []jiraIssue) bool {
		for _, is := range page {
			found = found || is.Key == key
		}
		return found
	})
	return found, err
}

// search reads up to max tasks of a query.
func (j *Jira) search(ctx context.Context, jql string, max int) ([]core.Task, error) {
	tasks, _, err := j.searchMemo(ctx, jql, max)
	return tasks, err
}

// searchMemo is search that also returns the comments it had to fetch beyond the search's page, by issue key:
// kept while the issue's updated and its number of comments stay the same, so a poll does not read the rest of
// a long thread again.
func (j *Jira) searchMemo(ctx context.Context, jql string, max int) ([]core.Task, map[string]commentMemo, error) {
	issues, err := j.pages(ctx, jql, max, jiraFields, nil)
	if err != nil {
		return nil, nil, err
	}
	j.mu.Lock()
	old := j.comments
	j.mu.Unlock()
	memos := map[string]commentMemo{}
	tasks := []core.Task{}
	for _, is := range issues {
		t := core.Task{
			ID: is.Key, Slug: is.Key, TrackerID: j.cfg.ID, Project: is.Fields.Project.Key,
			Title: is.Fields.Summary, Description: strings.TrimSpace(adfToText(is.Fields.Description)),
			URL: j.base + "/browse/" + is.Key, Labels: orEmpty(is.Fields.Labels), Components: []string{}, Comments: []core.Comment{},
			ClosingRef: is.Key, Assignees: []string{}, BlockedBy: is.blockedBy(),
		}
		priority := "?"
		if is.Fields.Priority != nil {
			priority = is.Fields.Priority.Name
			t.Priority = priority
		}
		t.Meta = fmt.Sprintf("Type: %s | Priority: %s", is.Fields.IssueType.Name, priority)
		for _, c := range is.Fields.Components {
			t.Components = append(t.Components, c.Name)
		}
		if is.Fields.Assignee != nil {
			t.Assignees = []string{is.Fields.Assignee.AccountID}
		}
		me, _ := j.Me(ctx)
		if is.Fields.Reporter != nil {
			t.Author = is.Fields.Reporter.DisplayName
			t.AuthorMember = is.Fields.Reporter.member(me, t.Assignees)
		}
		comments, total := is.Fields.Comment.Comments, is.Fields.Comment.Total
		if m := old[is.Key]; total > len(comments) && m.holds(is.Fields.Updated, total) {
			t.Comments, memos[is.Key] = slices.Clone(m.comments), m
		} else {
			fetched := false
			if total > len(comments) {
				if all, err := j.moreComments(ctx, is.Key, comments, total); err == nil {
					comments, fetched = all, true
				} else {
					t.CommentsUnread = true
				}
			}
			for _, c := range comments {
				t.Comments = append(t.Comments, core.Comment{Author: c.Author.DisplayName, Body: strings.TrimSpace(adfToText(c.Body)), Member: c.Author.member(me, t.Assignees)})
			}
			if fetched && len(comments) == total {
				memos[is.Key] = commentMemo{updated: is.Fields.Updated, count: total, comments: t.Comments}
			}
		}
		tasks = append(tasks, t)
	}
	return tasks, memos, nil
}

func (j *Jira) FetchMine(ctx context.Context) ([]core.Task, error) {
	jql := j.cfg.JQL
	if strings.TrimSpace(jql) == "" {
		jql = `assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC`
	}
	tasks, memos, err := j.searchMemo(ctx, jql, jiraMax)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	j.comments = memos
	j.mu.Unlock()
	return tasks, nil
}

func (j *Jira) AssignToMe(ctx context.Context, t core.Task) error {
	me, err := j.Me(ctx)
	if err != nil {
		return err
	}
	return j.do(ctx, "PUT", "/rest/api/3/issue/"+t.ID+"/assignee", map[string]any{"accountId": me}, nil)
}

func (j *Jira) Comment(ctx context.Context, t core.Task, text string) error {
	return j.do(ctx, "POST", "/rest/api/3/issue/"+t.ID+"/comment", map[string]any{"body": textToADF(text)}, nil)
}

func (j *Jira) IssueState(ctx context.Context, t core.Task) (string, error) {
	var is jiraIssue
	if err := j.do(ctx, "GET", "/rest/api/3/issue/"+t.ID+"?fields=status", nil, &is); err != nil {
		if isStatus(err, 404) {
			return "deleted", nil
		}
		return "", err
	}
	if is.Fields.Status.StatusCategory.Key == "done" {
		return "closed", nil
	}
	return "open", nil
}

// Move: the transition (or target status) set for the moment; after a merge «Done» when nothing is set and
// the workflow has it.
func (j *Jira) Move(ctx context.Context, t core.Task, moment string) (string, error) {
	tr := j.cfg.Transitions
	switch moment {
	case AtStart:
		return j.transition(ctx, t, pick(tr.OnStart, nil))
	case AtReview:
		return j.transition(ctx, t, pick(tr.OnSuccess, nil))
	case AtFailed:
		return j.transition(ctx, t, pick(tr.OnFailure, nil))
	case AtMerged:
		if tr.OnMerged != nil {
			return j.transition(ctx, t, *tr.OnMerged)
		}
		name, err := j.transition(ctx, t, "Done")
		if err != nil {
			return "", nil // no Done in this workflow: not an error
		}
		return name, nil
	}
	return "", nil
}

func (j *Jira) transition(ctx context.Context, t core.Task, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	var res struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := j.do(ctx, "GET", "/rest/api/3/issue/"+t.ID+"/transitions", nil, &res); err != nil {
		return "", err
	}
	for _, tr := range res.Transitions {
		if strings.EqualFold(tr.Name, name) || strings.EqualFold(tr.To.Name, name) {
			return name, j.do(ctx, "POST", "/rest/api/3/issue/"+t.ID+"/transitions", map[string]any{"transition": map[string]string{"id": tr.ID}}, nil)
		}
	}
	return "", fmt.Errorf("%s: Jira has no transition to \"%s\" from the current status", t.ID, name)
}

// adfToText flattens Atlassian Document Format into plain text good enough for a prompt.
func adfToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var node adfNode
	if json.Unmarshal(raw, &node) != nil {
		return ""
	}
	return node.text()
}

type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Attrs   map[string]any  `json:"attrs"`
	Content []adfNode       `json:"content"`
	_       json.RawMessage `json:"-"`
}

func (n adfNode) inner() string {
	var b strings.Builder
	for _, c := range n.Content {
		b.WriteString(c.text())
	}
	return b.String()
}

func (n adfNode) text() string {
	switch n.Type {
	case "text":
		return n.Text
	case "hardBreak":
		return "\n"
	case "mention":
		if s, ok := n.Attrs["text"].(string); ok {
			return s
		}
		return "@user"
	case "inlineCard":
		if s, ok := n.Attrs["url"].(string); ok {
			return s
		}
		return ""
	case "codeBlock":
		return "```\n" + n.inner() + "\n```\n\n"
	case "listItem":
		return "- " + strings.TrimSpace(n.inner()) + "\n"
	case "heading":
		level := 1
		if l, ok := n.Attrs["level"].(float64); ok {
			level = int(l)
		}
		return strings.Repeat("#", level) + " " + n.inner() + "\n\n"
	case "paragraph":
		return n.inner() + "\n\n"
	}
	return n.inner()
}

func textToADF(text string) map[string]any {
	paras := []any{}
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		content := []any{}
		for i, line := range strings.Split(para, "\n") {
			if i > 0 {
				content = append(content, map[string]any{"type": "hardBreak"})
			}
			if line != "" {
				content = append(content, map[string]any{"type": "text", "text": line})
			}
		}
		paras = append(paras, map[string]any{"type": "paragraph", "content": content})
	}
	return map[string]any{"type": "doc", "version": 1, "content": paras}
}
