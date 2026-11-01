package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Mako's own errors: one entry per kind across restarts, sent once to Mako's repository as an issue.
func TestFaults(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	if _, err := d.ReportFaults(ctx); err == nil {
		t.Fatal("reported with no errors")
	}
	d.Fault("grp/app#5", "MR не перевірено", errors.New("GET /projects/12: 502 Bad Gateway"))
	d.Fault("grp/app#7", "MR не перевірено", errors.New("GET /projects/13: 502 Bad Gateway"))
	d.Fault("gitlab", "пошук не вдався", errors.New("немає токена"))
	fs := d.Faults()
	if len(fs) != 2 || fs[0].What != "пошук не вдався" || fs[1].Count != 2 || fs[1].Task != "grp/app#7" || !strings.Contains(fs[1].Error, "/13:") {
		t.Fatalf("faults %+v", fs)
	}
	if !strings.Contains(fmtLogs(d.Logs("grp/app#5")), "MR не перевірено: GET /projects/12") {
		t.Errorf("not in the journal: %v", d.Logs("grp/app#5"))
	}

	// Mako's repository: the origin of the clone it is built from, on the tracker of that host.
	src := t.TempDir()
	git(t, src, "init", "-q")
	git(t, src, "remote", "add", "origin", f.base+"/grp/app.git")
	d.Source, d.Version = src, "1.2.3"
	id, err := d.ReportFaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := d.Store.Get(id)
	if !ok || r.Status != core.Snoozed || !strings.HasPrefix(r.Task.Title, "Помилки Mako: 2 види, 3 рази") {
		t.Fatalf("record %+v", r)
	}
	var desc string
	for _, is := range f.issues {
		if is["title"] == r.Task.Title {
			desc, _ = is["description"].(string)
		}
	}
	if !strings.Contains(desc, "`1.2.3`") || !strings.Contains(desc, "### MR не перевірено — 2 рази") || !strings.Contains(desc, "немає токена") {
		t.Errorf("description:\n%s", desc)
	}
	for _, f := range d.Faults() {
		if f.Reported != r.Task.URL {
			t.Errorf("not marked reported: %+v", f)
		}
	}
	if _, err := d.ReportFaults(ctx); err == nil {
		t.Error("the same errors reported twice")
	}
	// Happening again after the report makes it new, counted from there.
	d.Fault("gitlab", "пошук не вдався", errors.New("немає токена"))
	if fs := d.Faults(); fs[0].Reported != "" || fs[0].Count != 1 || fs[1].Reported == "" {
		t.Errorf("after the report %+v", fs)
	}
	if err := d.ClearFaults(); err != nil || len(d.Faults()) != 0 {
		t.Errorf("clear: %v %+v", err, d.Faults())
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{1: "вид", 2: "види", 5: "видів", 11: "видів", 12: "видів", 21: "вид", 23: "види"} {
		if got := plural(n, "вид", "види", "видів"); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}

func fmtLogs(ls []LogLine) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

// The network, the VPN or the service failing is not Mako's error: it is journaled, not kept.
func TestFaultsSkipTransient(t *testing.T) {
	d, _, _ := setup(t)
	dnsErr := &url.Error{Op: "Get", URL: "https://git.example.com/api", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "git.example.com", IsNotFound: true}}}
	outside := []error{
		dnsErr,
		fmt.Errorf("Get x: %w", syscall.ECONNRESET),
		fmt.Errorf("Get x: %w", syscall.EADDRNOTAVAIL),
		fmt.Errorf("claude: %w", context.Canceled),
		&trackers.HTTPError{Status: 502},
	}
	for _, err := range outside {
		d.Fault("gitlab", "пошук не вдався", err)
	}
	if fs := d.Faults(); len(fs) != 0 {
		t.Fatalf("kept %+v", fs)
	}
	if !strings.Contains(fmtLogs(d.Logs("gitlab")), "no such host") {
		t.Errorf("not in the journal: %v", d.Logs("gitlab"))
	}
	// Mako's own deadlines and kills, a bad token, a bad setting-free failure stay: they are for an agent to look at.
	d.Fault("a#1", "підсумок не написано", errors.New("claude: signal: killed: "))
	d.Fault("a#2", "рецензент не впорався", fmt.Errorf("claude: %w", context.DeadlineExceeded))
	d.Fault("gitlab", "пошук не вдався", &trackers.HTTPError{Status: 401})
	d.Fault("gitlab", "пошук не вдався", errors.New("немає токена"))
	if fs := d.Faults(); len(fs) != 4 {
		t.Fatalf("faults %+v", fs)
	}
	// A setting, not Mako: an unknown assignee is said on the board.
	d.Fault("github", "пошук не вдався", &trackers.HTTPError{Status: 422, Body: `{"field":"assignee"}`})
	if fs := d.Faults(); len(fs) != 4 {
		t.Fatalf("assignee kept: %+v", fs)
	}
}
