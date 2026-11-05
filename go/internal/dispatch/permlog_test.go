package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The journal groups the requests by the rule that would have spared them, the most asked first; a rule already
// allowed is left out, a dismissed one until an agent asks again; a request with no rule counts under its tool.
func TestPermissionJournal(t *testing.T) {
	d, _, _ := setup(t)
	d.NotePermission("a#1", "Bash", json.RawMessage(`{"command":"go vet ./..."}`), []string{"Bash(go vet:*)"})
	d.NotePermission("a#2", "Bash", json.RawMessage(`{"command":"cd go && go vet ./... && git push"}`), []string{"Bash(cd:*)", "Bash(go vet:*)", "Bash(git push:*)"})
	d.NotePermission("a#2", "WebFetch", json.RawMessage(`{"url":"https://example.com"}`), nil)
	show := func(allowed ...string) string {
		var out []string
		for _, g := range d.PermissionGroups(allowed) {
			out = append(out, fmt.Sprintf("%s %d/%d %s", g.Rule, g.Count, g.Agents, g.Example))
		}
		return strings.Join(out, " | ")
	}
	// The same count and second (made so: the clock may tick between the notes): by the rule.
	d.permMu.Lock()
	j := d.readPerms()
	for i := range j.Requests {
		j.Requests[i].At = 1000
	}
	d.writePerms(j)
	d.permMu.Unlock()
	want := "Bash(go vet:*) 2/2 cd go && go vet ./... && git push | Bash(cd:*) 1/1 cd go && go vet ./... && git push | Bash(git push:*) 1/1 cd go && go vet ./... && git push | WebFetch 1/1 https://example.com"
	if got := show(); got != want {
		t.Fatalf("groups\n got %s\nwant %s", got, want)
	}
	if got := show("Bash(go vet:*)", "Bash(cd:*)"); strings.Contains(got, "go vet:*") || strings.Contains(got, "cd:*") {
		t.Errorf("allowed rules shown: %s", got)
	}

	d.DismissPermissions([]string{"WebFetch"})
	if strings.Contains(show(), "WebFetch") {
		t.Error("a dismissed rule is shown")
	}
	// Asked again after it was dismissed: back, counting all its requests.
	time.Sleep(1100 * time.Millisecond) // the journal keeps seconds
	d.NotePermission("a#3", "WebFetch", json.RawMessage(`{"url":"https://example.org"}`), nil)
	if !strings.Contains(show(), "WebFetch 2/2 https://example.org") {
		t.Errorf("not back after a new request: %s", show())
	}

	// A long command is cut; the journal keeps the newest permLogMax requests.
	d.NotePermission("a#4", "Bash", json.RawMessage(`{"command":"`+strings.Repeat("x", 400)+`"}`), []string{"Bash(xx:*)"})
	for _, g := range d.PermissionGroups(nil) {
		if g.Rule == "Bash(xx:*)" && len([]rune(g.Example)) != 300 {
			t.Errorf("example of %d runes", len([]rune(g.Example)))
		}
	}
	for i := 0; i < permLogMax+10; i++ {
		d.NotePermission("a#5", "Read", nil, []string{"Read"})
	}
	b, _ := os.ReadFile(d.permsFile())
	j = permJournal{}
	json.Unmarshal(b, &j)
	if len(j.Requests) != permLogMax || j.Requests[0].Tool != "Read" {
		t.Errorf("%d requests kept, first %+v", len(j.Requests), j.Requests[0])
	}
}
