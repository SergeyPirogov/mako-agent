package core

import (
	"regexp"
	"strings"
	"time"
)

// SelfCheckStats: how «Check it yourself» went on the merged tasks the developer was asked to check by hand, and
// how many of each kind needed a fix after the merge: whether the agent's advice is worth following.
type SelfCheckStats struct {
	// Advised: merged tasks with steps to check; Checked: every step ticked at the branch as it was merged;
	// Unchecked: merged without that (skipped with a reason, or not ticked).
	Advised   int `json:"advised"`
	Checked   int `json:"checked"`
	Unchecked int `json:"unchecked"`
	// CheckedFixed, UncheckedFixed: of those, the ones a later bug-fix task points at.
	CheckedFixed   int `json:"checkedFixed"`
	UncheckedFixed int `json:"uncheckedFixed"`
}

// selfCheckStats: the merged tasks of the period (since; zero: all) that had a check to do, out of records; the
// later fixes are looked for among all of them.
func selfCheckStats(records []Record, since time.Time) SelfCheckStats {
	var st SelfCheckStats
	var fixes []Record
	for _, r := range records {
		if isFix(r) {
			fixes = append(fixes, r)
		}
	}
	for _, r := range records {
		if r.ReviewState != "merged" || r.CheckedAtMerge == "" || !since.IsZero() && !takenSince(r, since) {
			continue
		}
		st.Advised++
		fixed := fixedAfter(r, fixes)
		if r.CheckedAtMerge == "checked" {
			st.Checked++
			if fixed {
				st.CheckedFixed++
			}
		} else {
			st.Unchecked++
			if fixed {
				st.UncheckedFixed++
			}
		}
	}
	return st
}

// fixWord: a label or a tracker's type that says bug fix, by the whole word: "bug", "Type: Bug", "type::fix",
// not "debug" or "prefix". The same words as gitx.TaskType (it cannot be used here: gitx imports core).
var fixWord = regexp.MustCompile(`\b(bug|bugs|bugfix|fix|hotfix|defect|incident)\b`)

// isFix: the task is a bug fix, by its labels or its tracker's type ("Type: Bug").
func isFix(r Record) bool {
	for _, l := range r.Task.Labels {
		if fixWord.MatchString(strings.ToLower(l)) {
			return true
		}
	}
	return fixWord.MatchString(strings.ToLower(r.Task.Meta))
}

// fixedAfter: one of fixes came after r was merged and names it: its issue (the whole id or its URL, or #N in the
// same project) or its MR, in the title or the description.
func fixedAfter(r Record, fixes []Record) bool {
	merged, err := time.Parse(time.RFC3339Nano, r.MergedAt)
	if err != nil || len(fixes) == 0 {
		return false
	}
	var ref, short *regexp.Regexp
	if r.Task.ID != "" {
		ref = regexp.MustCompile(`(^|[^\w/.-])` + regexp.QuoteMeta(r.Task.ID) + `($|\D)`)
	}
	if _, n, ok := strings.Cut(r.Task.ID, "#"); ok && n != "" {
		short = regexp.MustCompile(`(^|[^\w/&])#` + regexp.QuoteMeta(n) + `($|\D)`)
	}
	for _, f := range fixes {
		if f.ID == r.ID {
			continue
		}
		seen, err := time.Parse(time.RFC3339Nano, orStr(f.CreatedAt, f.StartedAt))
		if err != nil || !seen.After(merged) {
			continue
		}
		text := f.Task.Title + "\n" + f.Task.Description
		switch {
		case containsRef(text, r.PRURL), containsRef(text, r.Task.URL),
			ref != nil && ref.MatchString(text),
			short != nil && f.Task.Project == r.Task.Project && short.MatchString(text):
			return true
		}
	}
	return false
}

// containsRef: text has ref (a URL) not followed by a digit: …/issues/5 is not …/issues/55.
func containsRef(text, ref string) bool {
	if ref == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], ref)
		if j < 0 {
			return false
		}
		end := i + j + len(ref)
		if end == len(text) || text[end] < '0' || text[end] > '9' {
			return true
		}
		i = end
	}
}

func orStr(s, or string) string {
	if s != "" {
		return s
	}
	return or
}
