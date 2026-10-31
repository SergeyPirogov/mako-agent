package notify

import "testing"

func TestShowSkipsMutedKinds(t *testing.T) {
	enabled, remote, muted := Enabled, Remote, Muted
	defer func() { Enabled, Remote, Muted = enabled, remote, muted }()
	Enabled = false
	got := make(chan string, 4)
	Remote = func(n Notice) { got <- n.Title }
	Muted = func(kind string) bool { return kind == NewTask }

	Inform(NewTask, "muted", "", "")
	Inform(Merged, "shown", "", "")
	Inform("", "untyped", "", "")
	seen := map[string]bool{<-got: true, <-got: true}
	if !seen["shown"] || !seen["untyped"] {
		t.Fatalf("shown %v", seen)
	}
	select {
	case title := <-got:
		t.Fatalf("%q was shown too", title)
	default:
	}
}
