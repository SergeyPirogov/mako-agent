// Package trackers talks to GitLab, GitHub and Jira Cloud: the tasks assigned to me, their status in the
// tracker, comments, and (GitLab, GitHub) the merge requests made from a task's branch.
package trackers

import (
	"context"
	"fmt"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Moment in a task's life that may move its issue to another status.
const (
	AtStart  = "start"  // handed to Orca
	AtReview = "review" // its MR appeared
	AtMerged = "merged"
	AtFailed = "failed"
)

type Tracker interface {
	ID() string
	Name() string
	Config() *config.Tracker
	// FetchMine: the open issues Mako should offer (mine, with the trigger label if one is set).
	FetchMine(ctx context.Context) ([]core.Task, error)
	// Me: the token owner, in the form of Task.Assignees.
	Me(ctx context.Context) (string, error)
	AssignToMe(ctx context.Context, t core.Task) error
	// Move puts the issue in the status set (or guessed) for a moment; it returns the status, "" when none.
	Move(ctx context.Context, t core.Task, moment string) (string, error)
	Comment(ctx context.Context, t core.Task, text string) error
	// IssueState: "open", "closed" or "deleted".
	IssueState(ctx context.Context, t core.Task) (string, error)
}

// CodeHost is where merge requests live (GitLab, GitHub), also for tasks from Jira.
type CodeHost interface {
	// FindReview: the newest MR/PR from branch in project ("group/app"), nil when there is none.
	FindReview(ctx context.Context, project, branch string) (*Review, error)
	// ReviewState: "open", "merged" or "closed".
	ReviewState(ctx context.Context, url string) (string, error)
	ReadReview(ctx context.Context, url string) (title, description string, err error)
	DescribeReview(ctx context.Context, url, title, description string) error
	// Host is the web host of the code ("gitlab.example.com").
	Host() string
}

type Review struct {
	URL    string
	Number int
	State  string // "open", "merged", "closed"
}

// New makes a tracker from its config; tokens come from .env and the environment.
func New(t *config.Tracker, cfg *config.Config) (Tracker, error) {
	switch t.Type {
	case "gitlab":
		return NewGitLab(t, cfg.Get("GITLAB_TOKEN"))
	case "github":
		return NewGitHub(t, GitHubToken(cfg))
	case "jira":
		return NewJira(t, cfg.Get("JIRA_EMAIL"), cfg.Get("JIRA_API_TOKEN"))
	}
	return nil, fmt.Errorf("невідомий тип трекера %q", t.Type)
}

func missing(name, where string) error {
	return fmt.Errorf("немає %s: додайте його в .env (%s)", name, where)
}
