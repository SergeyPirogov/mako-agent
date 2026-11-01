package trackers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func TestPriorityFromLabels(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   string
	}{
		{nil, ""},
		{[]string{"bug", "mako"}, ""},
		{[]string{"bug", "priority: High"}, "High"},
		{[]string{"priority::critical"}, "critical"},
		{[]string{"p1"}, "P1"},
		{[]string{"minor", "trivial"}, ""},
		{[]string{"bug", "urgent"}, "urgent"},
		{[]string{"high-priority", "priority/low"}, "low"},
	} {
		if got := priorityFromLabels(c.labels); got != c.want {
			t.Errorf("%v: got %q, want %q", c.labels, got, c.want)
		}
	}
}

func TestToTaskPriority(t *testing.T) {
	gh := (&GitHub{cfg: &config.Tracker{ID: "gh"}}).toTask("acme/web", ghIssue{Number: 3, Title: "x",
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "bug"}, {Name: "priority: high"}}})
	if gh.Priority != "high" {
		t.Errorf("github %q", gh.Priority)
	}
	gl := (&GitLab{cfg: &config.Tracker{ID: "gl"}}).toTask("grp/app", glIssue{IID: 1, Title: "x", Labels: []string{"priority::2"}})
	if gl.Priority != "2" {
		t.Errorf("gitlab %q", gl.Priority)
	}
	if none := (&GitLab{cfg: &config.Tracker{ID: "gl"}}).toTask("grp/app", glIssue{IID: 2}); none.Priority != "" {
		t.Errorf("none %q", none.Priority)
	}
}

func TestJiraNoPriority(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"issues":[{"key":"PAY-8","fields":{"summary":"s","issuetype":{"name":"Task"},"priority":null,"project":{"key":"PAY"}}}]}`))
	}))
	defer srv.Close()
	j, err := NewJira(&config.Tracker{ID: "jira", BaseURL: srv.URL}, "me@x", "tok")
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := j.FetchMine(context.Background())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%v %v", err, tasks)
	}
	if tasks[0].Priority != "" || tasks[0].Meta != "Type: Task | Priority: ?" {
		t.Errorf("%q %q", tasks[0].Priority, tasks[0].Meta)
	}
}
