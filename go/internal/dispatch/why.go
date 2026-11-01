package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Why is the answer to «Чому немає задачі?»: the task behind a link and each condition Mako's search puts on it.
type Why struct {
	ID      string           `json:"id,omitempty"`
	Title   string           `json:"title,omitempty"`
	URL     string           `json:"url,omitempty"`
	Tracker string           `json:"tracker,omitempty"`
	Checks  []trackers.Check `json:"checks"`
	// OnBoard: the task is already on the board (maybe in the archive); Verdict sums it up in a sentence.
	OnBoard bool   `json:"onBoard"`
	Verdict string `json:"verdict"`
}

var columnName = map[string]string{"new": "\"New\"", "doing": "\"In progress\"", "review": "\"In review\"", "done": "\"Done\"", "failed": "\"Failed\""}

// WhyNot looks the link up in the tracker it belongs to and says why the task is or is not on the board.
func (d *Dispatcher) WhyNot(ctx context.Context, link string) (Why, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return Why{}, fmt.Errorf("paste a link to the task")
	}
	var w Why
	for _, t := range d.TrackerList() {
		ex, ok := t.(trackers.Explainer)
		if !ok {
			continue
		}
		task, checks, err := ex.Why(ctx, link)
		if err != nil {
			return Why{}, err
		}
		if task == nil {
			continue
		}
		w = Why{ID: task.ID, Title: task.Title, URL: task.URL, Tracker: t.Name(), Checks: checks}
		if r, known := d.Store.Get(task.ID); known {
			w.OnBoard = true
			w.Verdict = "The task is already on the board, in the column " + columnName[r.Column(time.Now().UnixMilli())]
			if r.ArchivedAt > 0 {
				w.Verdict = "The task is already on the board, in the archive"
			}
			return w, nil
		}
		w.Checks = append(d.makoChecks(t), w.Checks...)
		break
	}
	if w.ID == "" {
		hosts := []string{}
		for _, t := range d.TrackerList() {
			hosts = append(hosts, t.Name()+" ("+trackerHost(t)+")")
		}
		if len(hosts) == 0 {
			return Why{}, fmt.Errorf("there is no tracker in the settings")
		}
		return Why{}, fmt.Errorf("the link is not from any tracker in the settings: %s", strings.Join(hosts, ", "))
	}
	failed := []string{}
	for _, c := range w.Checks {
		if !c.OK {
			failed = append(failed, c.Text)
		}
	}
	if len(failed) == 0 {
		w.Verdict = "The task matches the search: Mako will show it after the next tracker check"
		d.Wake()
	} else {
		w.Verdict = "Mako does not take the task: " + lowerFirst(failed[0])
	}
	return w, nil
}

// makoChecks: what of Mako itself stops a tracker's tasks from coming: the pause, a failing search.
func (d *Dispatcher) makoChecks(t trackers.Tracker) []trackers.Check {
	var checks []trackers.Check
	if _, paused := d.Status(); paused {
		checks = append(checks, trackers.Check{Text: "Task search is paused: unpause it on the board"})
	}
	s, ok := d.Searches()[t.ID()]
	switch {
	case ok && s.Error != "":
		checks = append(checks, trackers.Check{Text: "The last search in " + t.Name() + " failed: " + s.Error})
	case ok:
		checks = append(checks, trackers.Check{OK: true, Text: fmt.Sprintf("The last search in %s succeeded at %s", t.Name(), s.At.Format("15:04"))})
	}
	return checks
}

func trackerHost(t trackers.Tracker) string {
	if h, ok := t.(trackers.CodeHost); ok {
		return h.Host()
	}
	return strings.TrimPrefix(strings.TrimPrefix(t.Config().BaseURL, "https://"), "http://")
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}
