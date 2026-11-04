package gitx

import (
	"context"
	"strings"
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

// A clone URL with a token reaches neither the log nor an error.
func TestRedact(t *testing.T) {
	cases := map[string]string{
		"https://oauth2:glpat-SECRET@gitlab.com/grp/app.git":              "https://oauth2:***@gitlab.com/grp/app.git",
		"fatal: unable to access 'https://ghp_SECRET@github.com/o/r/': x": "fatal: unable to access 'https://***@github.com/o/r/': x",
		"clone HTTP://u:p@h/x and http://h/y":                             "clone HTTP://u:***@h/x and http://h/y",
		"git@github.com:o/r.git":                                          "git@github.com:o/r.git",
		"ssh://git@host/o/r.git":                                          "ssh://git@host/o/r.git",
		"https://github.com/o/r a@b.c":                                    "https://github.com/o/r a@b.c",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	_, err := Run(context.Background(), t.TempDir(), "clone", "https://user:SECRET@127.0.0.1:1/o/r.git", "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "user:***@") {
		t.Errorf("error %v", err)
	}
}
