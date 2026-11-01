package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Mako's own errors: what failed on Mako's side (a search, a comment, an MR check, a worktree left behind), kept
// apart from the journal and across restarts, one entry per kind with how often it happened, so they can go to
// Mako's own repository as one issue for an agent to look into («На аналіз»).

// Fault is one kind of error: the same step failing with the same message, numbers aside.
type Fault struct {
	Key   string    `json:"key"`
	What  string    `json:"what"`           // the step: "MR не перевірено"
	Error string    `json:"error"`          // the newest message
	Task  string    `json:"task,omitempty"` // the newest task (or tracker, "mako") it happened on
	Count int       `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// Reported: the issue it went to for analysis; it happening again after that makes it new.
	Reported string `json:"reported,omitempty"`
}

// MakoRepo: where the errors go when Mako does not know the clone it is built from.
const MakoRepo = "github.com/SergeyPirogov/mako-agent"

const (
	maxFaults    = 200
	maxFaultText = 500
)

var faultDigits = regexp.MustCompile(`\d+`)

// faultKey: the step and the message with its numbers (ids, ports, times) blurred, so one kind stays one entry.
func faultKey(what, msg string) string {
	return what + "\x00" + faultDigits.ReplaceAllString(clipText(msg, 200), "#")
}

// Fault journals a failed step as before and keeps it among Mako's errors.
func (d *Dispatcher) Fault(id, what string, err error) {
	msg := strings.TrimSpace(err.Error())
	d.Log(id, what+": "+msg)
	d.faultsMu.Lock()
	defer d.faultsMu.Unlock()
	all := d.readFaults()
	key, at := faultKey(what, msg), time.Now()
	i := -1
	for j := range all {
		if all[j].Key == key {
			i = j
			break
		}
	}
	if i < 0 {
		all = append(all, Fault{Key: key, What: what, First: at})
		i = len(all) - 1
	}
	f := &all[i]
	if f.Reported != "" {
		f.Reported, f.Count, f.First = "", 0, at
	}
	f.Error, f.Task, f.Last = clipText(msg, maxFaultText), id, at
	f.Count++
	sort.SliceStable(all, func(a, b int) bool { return all[a].Last.After(all[b].Last) })
	if len(all) > maxFaults {
		all = all[:maxFaults]
	}
	d.writeFaults(all)
}

// Faults: Mako's errors, the newest first.
func (d *Dispatcher) Faults() []Fault {
	d.faultsMu.Lock()
	defer d.faultsMu.Unlock()
	return d.readFaults()
}

// ClearFaults forgets them all («Очистити»).
func (d *Dispatcher) ClearFaults() error {
	d.faultsMu.Lock()
	defer d.faultsMu.Unlock()
	return d.writeFaults([]Fault{})
}

// ReportFaults opens one issue (a bug) in Mako's repository with the errors not reported yet, assigned to you
// and on the board, for an agent to look into; it returns the task's id.
func (d *Dispatcher) ReportFaults(ctx context.Context) (string, error) {
	d.faultsMu.Lock()
	fresh := []Fault{}
	for _, f := range d.readFaults() {
		if f.Reported == "" {
			fresh = append(fresh, f)
		}
	}
	d.faultsMu.Unlock()
	if len(fresh) == 0 {
		return "", fmt.Errorf("нових помилок немає")
	}
	trackerID, project, err := d.faultTarget(ctx)
	if err != nil {
		return "", err
	}
	n := trackers.NewIssue{Project: project, Title: faultTitle(fresh), Description: faultReport(fresh, d.Version), Bug: true}
	id, err := d.CreateTask(ctx, trackerID, n, false)
	if err != nil {
		return "", err
	}
	link := id
	if r, ok := d.Store.Get(id); ok && r.Task.URL != "" {
		link = r.Task.URL
	}
	seen := map[string]time.Time{}
	for _, f := range fresh {
		seen[f.Key] = f.Last
	}
	d.faultsMu.Lock()
	all := d.readFaults()
	for i := range all {
		// One that happened again while the issue was being written stays new.
		if last, ok := seen[all[i].Key]; ok && all[i].Reported == "" && all[i].Last.Equal(last) {
			all[i].Reported = link
		}
	}
	d.writeFaults(all)
	d.faultsMu.Unlock()
	d.Log("mako", fmt.Sprintf("помилки Mako (%d) передано на аналіз: %s", len(fresh), link))
	return id, nil
}

// faultTarget: the tracker and project of Mako's own repository — the origin of the clone Mako is built from,
// else MakoRepo — among the trackers that can open issues.
func (d *Dispatcher) faultTarget(ctx context.Context) (string, string, error) {
	host, project := "github.com", strings.ToLower(strings.TrimPrefix(MakoRepo, "github.com/"))
	if d.Source != "" {
		if origin := gitx.OriginURL(ctx, d.Source); gitx.RemoteProject(origin) != "" {
			host, project = remoteHost(origin), gitx.RemoteProject(origin)
		}
	}
	for _, t := range d.TrackerList() {
		ch, ok := t.(trackers.CodeHost)
		if _, creates := t.(trackers.Creator); ok && creates && ch.Host() == host {
			return t.ID(), project, nil
		}
	}
	return "", "", fmt.Errorf("немає трекера для %s/%s: додайте %s у налаштуваннях", host, project, map[bool]string{true: "GitHub", false: "GitLab"}[strings.Contains(host, "github")])
}

func faultTitle(fs []Fault) string {
	total := 0
	for _, f := range fs {
		total += f.Count
	}
	return fmt.Sprintf("Помилки Mako: %d %s, %d %s", len(fs), plural(len(fs), "вид", "види", "видів"), total, plural(total, "раз", "рази", "разів"))
}

// faultReport: the issue's description — what to do, where Mako runs, and every kind with its newest message.
func faultReport(fs []Fault, version string) string {
	if version == "" {
		version = "dev"
	}
	var b strings.Builder
	b.WriteString("Mako зібрав свої помилки на аналіз. Розберіться, чому кожна сталася, і виправте те, що можна " +
		"виправити в Mako (повідомлення без потреби, помилки, які варто обробити, справжні баги). " +
		"Те, що залежить від налаштувань чи мережі, опишіть у MR.\n\n")
	fmt.Fprintf(&b, "Mako `%s`, %s/%s\n\n", version, runtime.GOOS, runtime.GOARCH)
	for _, f := range fs {
		fmt.Fprintf(&b, "### %s — %d %s\n\n", f.What, f.Count, plural(f.Count, "раз", "рази", "разів"))
		fmt.Fprintf(&b, "- Перший раз: %s, останній: %s\n", f.First.Format("02.01.2006 15:04"), f.Last.Format("02.01.2006 15:04"))
		if f.Task != "" {
			fmt.Fprintf(&b, "- Останній раз на: `%s`\n", f.Task)
		}
		fmt.Fprintf(&b, "\n```\n%s\n```\n\n", strings.ReplaceAll(f.Error, "```", "'''"))
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// plural: the Ukrainian form for n — 1 вид, 2 види, 5 видів.
func plural(n int, one, few, many string) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return few
	}
	return many
}

// faultsFile: errors.json in the state folder; "" without one (a test's Mako), so it never lands in the
// current folder, a package of the repository.
func (d *Dispatcher) faultsFile() string {
	if d.Config().StateDir == "" {
		return ""
	}
	return filepath.Join(d.Config().StateDir, "errors.json")
}

func (d *Dispatcher) readFaults() []Fault {
	all := []Fault{}
	if d.faultsFile() == "" {
		return all
	}
	if b, err := os.ReadFile(d.faultsFile()); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

func (d *Dispatcher) writeFaults(all []Fault) error {
	if d.faultsFile() == "" {
		return nil
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.faultsFile()), 0o755); err != nil {
		return err
	}
	tmp := d.faultsFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.faultsFile())
}
