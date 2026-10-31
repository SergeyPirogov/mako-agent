package dispatch

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
)

// pushes catches what goes to the phone: «title | message | url», with « | <action> → <url>» for each button,
// and «withdraw <tag>» for a notification taken away.
func pushes(t *testing.T) func() []string {
	var mu sync.Mutex
	var got []string
	old, oldWithdraw := notify.Remote, notify.RemoteWithdraw
	notify.Remote = func(n notify.Notice) {
		mu.Lock()
		defer mu.Unlock()
		line := n.Title + " | " + n.Message + " | " + n.URL
		for _, a := range n.Actions {
			line += " | " + a.Title + " → " + a.URL
		}
		got = append(got, line)
	}
	notify.RemoteWithdraw = func(tag string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, "withdraw "+tag)
	}
	t.Cleanup(func() { notify.Remote, notify.RemoteWithdraw = old, oldWithdraw })
	return func() []string {
		time.Sleep(50 * time.Millisecond) // Inform pushes in a goroutine
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestSeenAfterAbsence(t *testing.T) {
	d, _, _ := setup(t)
	d.Cfg.StateDir = t.TempDir()
	d.Store.Update("grp/app#9", func(r *core.Record) {
		r.Status, r.PRURL, r.FinishedAt, r.Task.Title = core.Done, "https://mr/9", now(), "Оплата"
	})
	start := time.Now()
	if dg := d.Seen(start.Add(-10 * time.Hour)); dg != nil {
		t.Fatalf("first look: %+v", dg)
	}
	if dg := d.Seen(start.Add(-9 * time.Hour)); dg != nil {
		t.Fatalf("an hour later: %+v", dg)
	}
	dg := d.Seen(start)
	if dg == nil || len(dg.Review) != 1 || dg.Review[0].ID != "grp/app#9" || !dg.Review[0].New {
		t.Fatalf("after 9 hours: %+v", dg)
	}
	if dg := d.Seen(start.Add(time.Second)); dg != nil {
		t.Fatalf("told twice: %+v", dg)
	}
	// Kept over a restart of Mako.
	data, _ := os.ReadFile(filepath.Join(d.Cfg.StateDir, "digest.json"))
	if !strings.Contains(string(data), `"seen":`) {
		t.Fatalf("digest.json %s", data)
	}
	d.Cfg.Notifications.AwayHours = 0
	if dg := d.Seen(start.Add(48 * time.Hour)); dg != nil {
		t.Fatalf("turned off: %+v", dg)
	}
}

func TestMorningDigest(t *testing.T) {
	d, _, _ := setup(t)
	d.Cfg.StateDir = t.TempDir()
	d.BoardURL = "http://board"
	got := pushes(t)
	nine := time.Date(2026, 10, 31, 9, 0, 0, 0, time.Local)
	d.Store.Update("grp/app#9", func(r *core.Record) {
		r.Status, r.PRURL, r.ReviewState, r.Task.Title = core.Done, "https://mr/9", "merged", "Оплата"
		r.MergedAt = nine.Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	})
	d.Seen(nine.Add(-11 * time.Hour)) // last at the board last night

	d.morning(nine.Add(-time.Minute))
	if p := got(); len(p) != 0 {
		t.Fatalf("before its time: %v", p)
	}
	d.morning(nine.Add(5 * time.Minute))
	p := got()
	if len(p) != 1 || !strings.Contains(p[0], "змерджено: 1") || !strings.HasSuffix(p[0], "/#digest:"+strconv.FormatInt(nine.Add(-11*time.Hour).UnixMilli(), 10)) {
		t.Fatalf("morning push: %v", p)
	}
	d.morning(nine.Add(time.Hour))
	if p := got(); len(p) != 1 {
		t.Fatalf("sent twice: %v", p)
	}
	// The next morning you are at the board already: no push.
	d.Seen(nine.Add(24*time.Hour - 10*time.Minute))
	d.morning(nine.Add(24 * time.Hour))
	if p := got(); len(p) != 1 {
		t.Fatalf("at the board: %v", p)
	}
	// A Mac that slept until the evening does not get a morning digest.
	d.morning(nine.Add(48*time.Hour + morningLate + time.Minute))
	if p := got(); len(p) != 1 {
		t.Fatalf("in the evening: %v", p)
	}
}
