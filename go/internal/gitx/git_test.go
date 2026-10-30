package gitx

import (
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestTaskType(t *testing.T) {
	cases := []struct {
		task core.Task
		want string
	}{
		{core.Task{Meta: "Type: Task | Labels: x"}, "feature"},
		{core.Task{Meta: "Type: Issue"}, "fix"},
		{core.Task{Meta: "Type: Incident"}, "fix"},
		{core.Task{Meta: "Type: Issue", Labels: []string{"feature"}}, "feature"},
		{core.Task{Meta: "Type: Bug | Priority: High"}, "fix"},
		{core.Task{Meta: "Type: Story | Priority: High"}, "feature"},
		{core.Task{Title: "Fix login redirect"}, "fix"},
		{core.Task{Labels: []string{"bug"}}, "fix"},
	}
	for _, c := range cases {
		if got := TaskType(c.task); got != c.want {
			t.Errorf("%+v: %s, want %s", c.task, got, c.want)
		}
	}
}

func TestBranchName(t *testing.T) {
	task := core.Task{ID: "grp/app#12", Slug: "app-12", Title: "Додати експорт у CSV", Meta: "Type: Task"}
	if got := BranchName("{type}/{id}-{title}", task, 1); got != "feature/12-dodaty-eksport-u-csv" {
		t.Errorf("got %s", got)
	}
	if got := BranchName("{type}/{id}-{title}", task, 2); got != "feature/12-dodaty-eksport-u-csv-try2" {
		t.Errorf("retry: %s", got)
	}
	jira := core.Task{ID: "PAY-7", Slug: "PAY-7", Title: "Refund fails", Meta: "Type: Bug | Priority: High"}
	if got := BranchName("{type}/{id}-{title}", jira, 1); got != "fix/PAY-7-refund-fails" {
		t.Errorf("jira: %s", got)
	}
}

func TestRemoteProject(t *testing.T) {
	for remote, want := range map[string]string{
		"git@gitlab.example.com:group/sub/app.git": "group/sub/app",
		"https://github.com/acme/web.git":          "acme/web",
		"ssh://git@host:2222/team/svc.git":         "team/svc",
	} {
		if got := RemoteProject(remote); got != want {
			t.Errorf("%s: %s, want %s", remote, got, want)
		}
	}
}
