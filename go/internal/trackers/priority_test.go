package trackers

import "testing"

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
		{[]string{"bug", "urgent"}, "urgent"},
		{[]string{"high-priority", "priority/low"}, "low"},
	} {
		if got := priorityFromLabels(c.labels); got != c.want {
			t.Errorf("%v: got %q, want %q", c.labels, got, c.want)
		}
	}
}
