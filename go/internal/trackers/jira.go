package trackers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
}

func NewJira(t *config.Tracker, email, token string) (*Jira, error) {
	if t.BaseURL == "" {
		return nil, fmt.Errorf("немає адреси Jira: вкажіть її в налаштуваннях або JIRA_BASE_URL у .env")
	}
	if email == "" || token == "" {
		return nil, missing("JIRA_EMAIL і JIRA_API_TOKEN", "токен: id.atlassian.com → Security → API tokens")
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
			return "Jira не прийняла дані входу (401): перевірте JIRA_EMAIL і API-токен (id.atlassian.com → Security → API tokens)"
		case 403:
			return "Jira заборонила дію (403): у вашого акаунта немає потрібних прав у цьому проєкті"
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
		Reporter *struct {
			DisplayName string `json:"displayName"`
		} `json:"reporter"`
		Comment struct {
			Comments []struct {
				Body   json.RawMessage `json:"body"`
				Author struct {
					DisplayName string `json:"displayName"`
				} `json:"author"`
			} `json:"comments"`
		} `json:"comment"`
		Status struct {
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
	} `json:"fields"`
}

func (j *Jira) search(ctx context.Context, jql string, max int) ([]core.Task, error) {
	var res struct {
		Issues []jiraIssue `json:"issues"`
	}
	body := map[string]any{"jql": jql, "maxResults": max, "fields": []string{"summary", "description", "issuetype", "priority", "project", "comment", "labels", "components", "assignee", "reporter"}}
	if err := j.do(ctx, "POST", "/rest/api/3/search/jql", body, &res); err != nil {
		return nil, err
	}
	tasks := []core.Task{}
	for _, is := range res.Issues {
		t := core.Task{
			ID: is.Key, Slug: is.Key, TrackerID: j.cfg.ID, Project: is.Fields.Project.Key,
			Title: is.Fields.Summary, Description: strings.TrimSpace(adfToText(is.Fields.Description)),
			URL: j.base + "/browse/" + is.Key, Labels: orEmpty(is.Fields.Labels), Components: []string{}, Comments: []core.Comment{},
			ClosingRef: is.Key, Assignees: []string{},
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
		for _, c := range is.Fields.Comment.Comments {
			t.Comments = append(t.Comments, core.Comment{Author: c.Author.DisplayName, Body: strings.TrimSpace(adfToText(c.Body))})
		}
		if is.Fields.Assignee != nil {
			t.Assignees = []string{is.Fields.Assignee.AccountID}
		}
		if is.Fields.Reporter != nil {
			t.Author = is.Fields.Reporter.DisplayName
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (j *Jira) FetchMine(ctx context.Context) ([]core.Task, error) {
	jql := j.cfg.JQL
	if strings.TrimSpace(jql) == "" {
		jql = `assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC`
	}
	return j.search(ctx, jql, 50)
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
	return "", fmt.Errorf("%s: у Jira немає переходу в «%s» з поточного статусу", t.ID, name)
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
