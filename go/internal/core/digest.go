package core

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Digest is what happened while you were away: what waits for you, the MRs to merge, what failed and what
// got merged, each with a link to its task.
type Digest struct {
	Since   int64        `json:"since"` // ms
	Until   int64        `json:"until"` // ms
	Waiting []DigestItem `json:"waiting"`
	Review  []DigestItem `json:"review"`
	Failed  []DigestItem `json:"failed"`
	Merged  []DigestItem `json:"merged"`
}

type DigestItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Note  string `json:"note,omitempty"` // why it waits, why it failed
	MR    string `json:"mr,omitempty"`
	New   bool   `json:"new,omitempty"` // an MR opened while you were away
	At    int64  `json:"at,omitempty"`  // ms: when it came to this
}

// Count of everything in the digest; 0: nothing waits and nothing happened.
func (d Digest) Count() int { return len(d.Waiting) + len(d.Review) + len(d.Failed) + len(d.Merged) }

// Line: the digest in one line, for a push.
func (d Digest) Line() string {
	parts := []string{}
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, what+": "+strconv.Itoa(n))
		}
	}
	add(len(d.Waiting), "чекає на вас")
	add(len(d.Review), "MR до мерджу")
	add(len(d.Failed), "не вдалося")
	add(len(d.Merged), "змерджено")
	return strings.Join(parts, " · ")
}

// BuildDigest sums the records up for the time from since to now. waiting: the tasks whose agent waits for you
// (or finished its turn without an MR), with the reason. Waiting and open MRs are listed whenever they appeared;
// failures and merges only when they happened in the time.
func BuildDigest(records []Record, waiting map[string]string, since, now time.Time) Digest {
	d := Digest{Since: since.UnixMilli(), Until: now.UnixMilli(), Waiting: []DigestItem{}, Review: []DigestItem{}, Failed: []DigestItem{}, Merged: []DigestItem{}}
	in := func(s string) (int64, bool) {
		t, err := time.Parse(time.RFC3339Nano, s)
		return t.UnixMilli(), err == nil && !t.Before(since) && !t.After(now)
	}
	for _, r := range records {
		item := DigestItem{ID: r.ID, Title: r.Task.Title, MR: r.PRURL}
		if r.ArchivedAt > 0 && r.ReviewState != "merged" {
			continue
		}
		switch col := r.Column(now.UnixMilli()); {
		case r.ReviewState == "merged":
			if at, ok := in(r.MergedAt); ok {
				item.At = at
				d.Merged = append(d.Merged, item)
			}
		case col == "failed":
			if at, ok := in(r.UpdatedAt); ok {
				item.At, item.Note = at, r.Error
				d.Failed = append(d.Failed, item)
			}
		case col == "review":
			item.At, item.New = in(r.FinishedAt)
			d.Review = append(d.Review, item)
		case col == "doing":
			if why, ok := waiting[r.ID]; ok {
				item.Note = why
				d.Waiting = append(d.Waiting, item)
			}
		case col == "new" && !r.Postponed(now.UnixMilli()):
			item.Note, item.MR = "нова задача: взяти в роботу?", ""
			d.Waiting = append(d.Waiting, item)
		}
	}
	for _, xs := range [][]DigestItem{d.Waiting, d.Review, d.Failed, d.Merged} {
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].At > xs[j].At })
	}
	return d
}
